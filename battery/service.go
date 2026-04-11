package battery

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
)

func NewService(config *ServiceConfig, batteryConfig *BatteryConfiguration, logger *log.Logger, logLevel LogLevel, debugMode bool) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Service{
		config:        config,
		batteryConfig: batteryConfig,
		logger:        slog.New(NewServiceHandler(logger.Writer(), logLevel)),
		stdLogger:     logger,
		ctx:           ctx,
		cancel:        cancel,
		debug:         debugMode,
		vehicleState:  VehicleStateStandby,
		readers:       make([]*BatteryReader, len(batteryConfig.Readers)),
	}

	s.redis = redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", config.RedisServerAddress, config.RedisServerPort),
	})

	if err := s.redis.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %v", err)
	}

	var activeReadersCreated bool
	for i, readerConfig := range batteryConfig.Readers {
		if !readerConfig.Enabled {
			s.logger.Info(fmt.Sprintf("Reader %d is disabled in configuration", readerConfig.Index))
			continue
		}

		reader, err := NewBatteryReader(readerConfig.Index, readerConfig.Role, readerConfig.DeviceName, readerConfig.LogLevel, s)
		if err != nil {
			s.logger.Error(fmt.Sprintf("Failed to create reader %d: %v", readerConfig.Index, err))
			continue
		}
		s.readers[i] = reader

		if readerConfig.Role == BatteryRoleActive {
			activeReadersCreated = true
		}
	}

	if !activeReadersCreated {
		return nil, fmt.Errorf("failed to create any active role readers - service cannot function")
	}

	return s, nil
}

func (s *Service) Start() error {
	s.logger.Info("Starting battery service")

	s.loadBoolSetting(s.ignoreSeatboxSettingSpec())
	s.loadBoolSetting(s.keepActiveOnSeatboxOpenSettingSpec())
	s.loadUint64Setting(s.maxVoltageDeltaSettingSpec())
	s.loadDualBatterySetting()

	if s.config.DangerouslyIgnoreSeatbox.Load() && s.config.KeepActiveOnSeatboxOpen.Load() {
		s.logger.Warn("Both dangerously-ignore-seatbox and keep-active-on-seatbox-open are set; dangerously-ignore-seatbox wins (superset)")
	}

	s.loadInitialVehicleState()

	go s.runRedisSubscriber()

	for _, reader := range s.readers {
		if reader != nil {
			if err := reader.Start(); err != nil {
				s.logger.Error(fmt.Sprintf("Failed to start reader %d: %v", reader.index, err))
				continue
			}
		}
	}

	s.logger.Info("Battery service started successfully")
	return nil
}

func (s *Service) Stop() {
	s.logger.Info("Stopping battery service")

	s.cancel()

	var wg sync.WaitGroup
	for _, reader := range s.readers {
		if reader != nil {
			wg.Add(1)
			go func(r *BatteryReader) {
				defer wg.Done()
				r.Stop()
			}(reader)
		}
	}

	wg.Wait()

	if s.redis != nil {
		s.redis.Close()
	}

	s.logger.Info("Battery service stopped")
}

func (s *Service) SetBatteryEnabled(index int, enabled bool) error {
	if index >= len(s.readers) || s.readers[index] == nil {
		return fmt.Errorf("invalid battery index: %d", index)
	}

	s.readers[index].SetEnabled(enabled)
	return nil
}

func (s *Service) runRedisSubscriber() {
	s.logger.Info("Starting Redis subscriber for channels: vehicle(state, seatbox:lock), settings")

	pubsub := s.redis.Subscribe(s.ctx,
		"vehicle",
		"settings",
	)
	defer pubsub.Close()

	_, err := pubsub.Receive(s.ctx)
	if err != nil {
		s.logger.Error(fmt.Sprintf("Failed to establish Redis subscription: %v", err))
		s.stdLogger.Fatal("Redis connection failed, exiting to allow systemd restart")
	}
	s.logger.Info("Redis subscription established successfully")

	ch := pubsub.Channel()
	s.logger.Debug("Listening for Redis messages...")

	for {
		select {
		case msg := <-ch:
			if msg == nil {
				s.logger.Error("Redis channel closed unexpectedly")
				s.stdLogger.Fatal("Redis connection lost, exiting to allow systemd restart")
			}
			s.logger.Debug(fmt.Sprintf("Received Redis message: channel=%s, payload=%s", msg.Channel, msg.Payload))

			switch msg.Channel {
			case "vehicle":
				switch msg.Payload {
				case "state":
					s.handleVehicleStateMessage()
				case "seatbox:lock":
					s.handleSeatboxUpdate()
				}
			case "settings":
				switch msg.Payload {
				case "scooter.battery-ignores-seatbox":
					s.reloadBoolSetting(s.ignoreSeatboxSettingSpec())
				case "scooter.battery-keep-active-on-seatbox-open":
					s.reloadBoolSetting(s.keepActiveOnSeatboxOpenSettingSpec())
				case "scooter.max-voltage-delta":
					s.reloadUint64Setting(s.maxVoltageDeltaSettingSpec())
				case "scooter.dual-battery":
					s.handleDualBatterySettingChange()
				}
			default:
				s.logger.Warn(fmt.Sprintf("Unknown Redis channel: %s", msg.Channel))
			}

		case <-s.ctx.Done():
			s.logger.Info("Redis subscriber context cancelled")
			return
		}
	}
}

// loadInitialVehicleState reads the vehicle state and seatbox lock from Redis
// before the readers start, so each reader's init completes against real
// values instead of the 5-second timeout fallback. Without this, a reader
// waking up between vehicle-service publishes would default to
// seatboxLockClosed=false, which is the "seatbox open" path and walks a fresh
// tag through StateSendOff/StateSendOpened instead of straight to heartbeat.
func (s *Service) loadInitialVehicleState() {
	vehicleState := VehicleStateStandby
	if raw, err := s.redis.HGet(s.ctx, "vehicle", "state").Result(); err == nil {
		vehicleState = VehicleState(raw)
	} else if err != redis.Nil {
		s.logger.Warn(fmt.Sprintf("Failed to read initial vehicle state: %v", err))
	}
	s.vehicleState = vehicleState
	s.logger.Info(fmt.Sprintf("Initial vehicle state: %s", vehicleState))

	// Default to open (false) when the key is missing, matching the old
	// 5-second init timeout behaviour so we stay on the conservative path.
	seatboxClosed := false
	if raw, err := s.redis.HGet(s.ctx, "vehicle", "seatbox:lock").Result(); err == nil {
		seatboxClosed = (raw == "closed")
	} else if err != redis.Nil {
		s.logger.Warn(fmt.Sprintf("Failed to read initial seatbox lock state: %v", err))
	}
	s.logger.Info(fmt.Sprintf("Initial seatbox lock: closed=%t", seatboxClosed))

	for _, reader := range s.readers {
		if reader != nil {
			reader.SendVehicleStateChange(vehicleState)
			reader.SendSeatboxLockChange(seatboxClosed)
		}
	}
}

func (s *Service) handleVehicleStateMessage() {
	vehicleState, err := s.redis.HGet(s.ctx, "vehicle", "state").Result()
	if err != nil {
		s.logger.Error(fmt.Sprintf("Failed to fetch vehicle state: %v", err))
		return
	}

	newState := VehicleState(vehicleState)
	s.logger.Info(fmt.Sprintf("Vehicle state changed: %s", newState))

	s.vehicleState = newState

	for _, reader := range s.readers {
		if reader != nil {
			reader.SendVehicleStateChange(newState)
		}
	}
}

func (s *Service) handleSeatboxUpdate() {
	seatboxLock, err := s.redis.HGet(s.ctx, "vehicle", "seatbox:lock").Result()
	if err != nil {
		s.logger.Error(fmt.Sprintf("Failed to fetch seatbox lock state: %v", err))
		return
	}

	closed := (seatboxLock == "closed")
	s.logger.Info(fmt.Sprintf("Seatbox lock changed: %s (closed=%t)", seatboxLock, closed))

	for _, reader := range s.readers {
		if reader != nil {
			reader.SendSeatboxLockChange(closed)
		}
	}
}

// ----- Hot-reloaded settings plumbing -----

// redisBoolSetting describes a bool Redis setting that can be loaded at
// startup and hot-reloaded from a Redis "settings" pub/sub notification.
type redisBoolSetting struct {
	key      string
	target   *atomic.Bool
	onChange func(oldValue, newValue bool) // optional; called on reload only
}

// redisUint64Setting is the uint64-valued counterpart to redisBoolSetting.
// valueSuffix is appended to the value when logging, e.g. " mV".
type redisUint64Setting struct {
	key         string
	target      *atomic.Uint64
	valueSuffix string
	onChange    func(oldValue, newValue uint64)
}

func parseBoolSetting(raw string) (bool, bool) {
	switch raw {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func (s *Service) loadBoolSetting(spec redisBoolSetting) {
	raw, err := s.redis.HGet(s.ctx, "settings", spec.key).Result()
	if err != nil {
		if err != redis.Nil {
			s.logger.Warn(fmt.Sprintf("Failed to load %s setting: %v", spec.key, err))
		}
		return
	}
	value, ok := parseBoolSetting(raw)
	if !ok {
		s.logger.Warn(fmt.Sprintf("Invalid %s value: %q (must be 'true' or 'false')", spec.key, raw))
		return
	}
	spec.target.Store(value)
	s.logger.Info(fmt.Sprintf("Loaded %s setting: %t", spec.key, value))
}

func (s *Service) reloadBoolSetting(spec redisBoolSetting) {
	raw, err := s.redis.HGet(s.ctx, "settings", spec.key).Result()
	if err != nil {
		s.logger.Error(fmt.Sprintf("Failed to fetch %s setting: %v", spec.key, err))
		return
	}
	value, ok := parseBoolSetting(raw)
	if !ok {
		s.logger.Warn(fmt.Sprintf("Invalid %s value: %q (must be 'true' or 'false')", spec.key, raw))
		return
	}
	oldValue := spec.target.Swap(value)
	if oldValue == value {
		return
	}
	s.logger.Info(fmt.Sprintf("%s setting changed: %t -> %t", spec.key, oldValue, value))
	if spec.onChange != nil {
		spec.onChange(oldValue, value)
	}
}

func formatValueSuffix(suffix string) string {
	if suffix == "" {
		return ""
	}
	return " " + suffix
}

func (s *Service) loadUint64Setting(spec redisUint64Setting) {
	raw, err := s.redis.HGet(s.ctx, "settings", spec.key).Result()
	if err != nil {
		if err != redis.Nil {
			s.logger.Warn(fmt.Sprintf("Failed to load %s setting: %v", spec.key, err))
		}
		return
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		s.logger.Warn(fmt.Sprintf("Invalid %s value: %q (must be a non-negative integer)", spec.key, raw))
		return
	}
	spec.target.Store(value)
	s.logger.Info(fmt.Sprintf("Loaded %s setting: %d%s", spec.key, value, formatValueSuffix(spec.valueSuffix)))
}

func (s *Service) reloadUint64Setting(spec redisUint64Setting) {
	raw, err := s.redis.HGet(s.ctx, "settings", spec.key).Result()
	if err != nil {
		s.logger.Error(fmt.Sprintf("Failed to fetch %s setting: %v", spec.key, err))
		return
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		s.logger.Warn(fmt.Sprintf("Invalid %s value: %q (must be a non-negative integer)", spec.key, raw))
		return
	}
	oldValue := spec.target.Swap(value)
	if oldValue == value {
		return
	}
	suffix := formatValueSuffix(spec.valueSuffix)
	s.logger.Info(fmt.Sprintf("%s setting changed: %d%s -> %d%s", spec.key, oldValue, suffix, value, suffix))
	if spec.onChange != nil {
		spec.onChange(oldValue, value)
	}
}

// ----- Setting specs -----

func (s *Service) ignoreSeatboxSettingSpec() redisBoolSetting {
	return redisBoolSetting{
		key:    "scooter.battery-ignores-seatbox",
		target: &s.config.DangerouslyIgnoreSeatbox,
		onChange: func(_, _ bool) {
			s.restartActiveReaders()
		},
	}
}

// keepActiveOnSeatboxOpenSettingSpec is the opt-in safer sibling of the
// dangerously-ignore-seatbox flag. Reloading it deliberately restarts any
// mid-cycle active reader so the new value takes effect at once, unlike the
// latch-change restart suppression over in reader.go which protects a
// running battery from being bounced through StateSendOff.
func (s *Service) keepActiveOnSeatboxOpenSettingSpec() redisBoolSetting {
	return redisBoolSetting{
		key:    "scooter.battery-keep-active-on-seatbox-open",
		target: &s.config.KeepActiveOnSeatboxOpen,
		onChange: func(_, _ bool) {
			s.restartActiveReaders()
		},
	}
}

func (s *Service) restartActiveReaders() {
	for _, reader := range s.readers {
		if reader != nil && reader.role == BatteryRoleActive {
			reader.triggerRestart()
		}
	}
}

func (s *Service) maxVoltageDeltaSettingSpec() redisUint64Setting {
	return redisUint64Setting{
		key:         "scooter.max-voltage-delta",
		target:      &s.config.MaxVoltageDelta,
		valueSuffix: "mV",
	}
}

// checkVoltageDelta reads both battery voltages from Redis and checks if the
// difference is within acceptable limits. Returns true if the delta is OK or
// if voltage data is unavailable (can't check). Also returns true if the
// threshold is set to 0 (disabled).
func (s *Service) checkVoltageDelta() (ok bool, delta uint64) {
	maxDelta := s.config.MaxVoltageDelta.Load()
	if maxDelta == 0 {
		return true, 0
	}

	v0, err := s.redis.HGet(s.ctx, "battery:0", "voltage").Uint64()
	if err != nil || v0 == 0 {
		return true, 0
	}
	v1, err := s.redis.HGet(s.ctx, "battery:1", "voltage").Uint64()
	if err != nil || v1 == 0 {
		return true, 0
	}

	if v0 > v1 {
		delta = v0 - v1
	} else {
		delta = v1 - v0
	}
	return delta <= maxDelta, delta
}

func (s *Service) loadDualBatterySetting() {
	setting, err := s.redis.HGet(s.ctx, "settings", "scooter.dual-battery").Result()
	if err != nil {
		if err != redis.Nil {
			s.logger.Warn(fmt.Sprintf("Failed to load scooter.dual-battery setting: %v", err))
		}
		return
	}

	if setting != "true" && setting != "false" {
		s.logger.Warn(fmt.Sprintf("Invalid scooter.dual-battery value: %q (must be 'true' or 'false')", setting))
		return
	}

	dualBattery := (setting == "true")

	// Update battery 1 role based on setting
	if len(s.batteryConfig.Readers) > 1 {
		newRole := BatteryRoleInactive
		if dualBattery {
			newRole = BatteryRoleActive

			// Check voltage delta before activating
			if ok, delta := s.checkVoltageDelta(); !ok {
				s.logger.Warn(fmt.Sprintf("Voltage delta too large (%dmV > %dmV) - refusing to activate battery 1", delta, s.config.MaxVoltageDelta.Load()))
				newRole = BatteryRoleInactive
			}
		}
		s.batteryConfig.Readers[1].Role = newRole

		// Also update the reader if it was already created
		if len(s.readers) > 1 && s.readers[1] != nil {
			s.readers[1].role = newRole
			if newRole == BatteryRoleActive {
				s.readers[1].enabled = true
			} else {
				s.readers[1].enabled = false
			}
		}

		s.logger.Info(fmt.Sprintf("Loaded scooter.dual-battery setting: %t (battery 1 role: %v)", dualBattery, newRole))
	}
}

func (s *Service) handleDualBatterySettingChange() {
	setting, err := s.redis.HGet(s.ctx, "settings", "scooter.dual-battery").Result()
	if err != nil {
		s.logger.Error(fmt.Sprintf("Failed to fetch scooter.dual-battery setting: %v", err))
		return
	}

	if setting != "true" && setting != "false" {
		s.logger.Warn(fmt.Sprintf("Invalid scooter.dual-battery value: %q (must be 'true' or 'false')", setting))
		return
	}

	dualBattery := (setting == "true")

	// Check if battery 1 exists
	if len(s.readers) <= 1 || s.readers[1] == nil {
		s.logger.Warn("Battery 1 reader not available, cannot change dual-battery setting")
		return
	}

	reader := s.readers[1]
	oldRole := reader.role
	newRole := BatteryRoleInactive
	if dualBattery {
		newRole = BatteryRoleActive
	}

	if oldRole == newRole {
		s.logger.Debug(fmt.Sprintf("Battery dual-battery setting unchanged: %t (role: %v)", dualBattery, newRole))
		return
	}

	// Check voltage delta before activating
	if newRole == BatteryRoleActive {
		if ok, delta := s.checkVoltageDelta(); !ok {
			s.logger.Warn(fmt.Sprintf("Voltage delta too large (%dmV > %dmV) - refusing to activate battery 1", delta, s.config.MaxVoltageDelta.Load()))
			return
		}
		reader.voltageDeltaBlocked = false
	}

	reader.role = newRole
	s.batteryConfig.Readers[1].Role = newRole

	s.logger.Info(fmt.Sprintf("Scooter dual-battery setting changed: %v -> %v", oldRole, newRole))

	// Update enabled state based on new role
	if newRole == BatteryRoleInactive {
		// Inactive batteries are always disabled
		reader.SetEnabled(false)
	} else {
		// Active batteries follow seatbox state (unless ignoring seatbox)
		if s.config.DangerouslyIgnoreSeatbox.Load() {
			reader.SetEnabled(true)
		} else {
			seatboxLock, err := s.redis.HGet(s.ctx, "vehicle", "seatbox:lock").Result()
			if err == nil {
				closed := (seatboxLock == "closed")
				reader.SetEnabled(closed)
			}
		}
	}

	// Trigger restart to apply the new role
	reader.triggerRestart()
}
