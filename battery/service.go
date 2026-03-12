package battery

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"sync"

	ipc "github.com/librescoot/redis-ipc"
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

	client, err := ipc.New(
		ipc.WithAddress(config.RedisServerAddress),
		ipc.WithPort(int(config.RedisServerPort)),
		ipc.WithLogger(slog.New(NewServiceHandler(logger.Writer(), logLevel))),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to connect to Redis: %v", err)
	}
	s.ipc = client

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

	s.loadIgnoreSeatboxSetting()
	s.loadMaxVoltageDeltaSetting()
	s.loadDualBatterySetting()

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

	if s.ipc != nil {
		s.ipc.Close()
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

	vehicleWatcher := s.ipc.NewHashWatcher("vehicle")
	vehicleWatcher.OnField("state", func(value string) error {
		s.handleVehicleStateChange(value)
		return nil
	})
	vehicleWatcher.OnField("seatbox:lock", func(value string) error {
		s.handleSeatboxChange(value)
		return nil
	})

	settingsWatcher := s.ipc.NewHashWatcher("settings")
	settingsWatcher.OnField("scooter.battery-ignores-seatbox", func(value string) error {
		s.handleIgnoreSeatboxSettingChange()
		return nil
	})
	settingsWatcher.OnField("scooter.dual-battery", func(value string) error {
		s.handleDualBatterySettingChange()
		return nil
	})
	settingsWatcher.OnField("scooter.max-voltage-delta", func(value string) error {
		s.handleMaxVoltageDeltaSettingChange()
		return nil
	})

	if err := vehicleWatcher.Start(); err != nil {
		s.logger.Error(fmt.Sprintf("Failed to start vehicle watcher: %v", err))
		s.stdLogger.Fatal("Redis connection failed, exiting to allow systemd restart")
	}
	if err := settingsWatcher.Start(); err != nil {
		s.logger.Error(fmt.Sprintf("Failed to start settings watcher: %v", err))
		s.stdLogger.Fatal("Redis connection failed, exiting to allow systemd restart")
	}

	s.logger.Info("Redis subscription established successfully")

	<-s.ctx.Done()
	s.logger.Info("Redis subscriber context cancelled")

	vehicleWatcher.Stop()
	settingsWatcher.Stop()
}

func (s *Service) handleVehicleStateChange(vehicleState string) {
	newState := VehicleState(vehicleState)
	s.logger.Info(fmt.Sprintf("Vehicle state changed: %s", newState))

	s.vehicleState = newState

	for _, reader := range s.readers {
		if reader != nil {
			reader.SendVehicleStateChange(newState)
		}
	}
}

func (s *Service) handleSeatboxChange(seatboxLock string) {
	closed := (seatboxLock == "closed")
	s.logger.Info(fmt.Sprintf("Seatbox lock changed: %s (closed=%t)", seatboxLock, closed))

	for _, reader := range s.readers {
		if reader != nil {
			reader.SendSeatboxLockChange(closed)
		}
	}
}

func (s *Service) loadIgnoreSeatboxSetting() {
	setting, err := s.ipc.HGet("settings", "scooter.battery-ignores-seatbox")
	if err != nil {
		if err != ipc.ErrNil {
			s.logger.Warn(fmt.Sprintf("Failed to load scooter.battery-ignores-seatbox setting: %v", err))
		}
		return
	}

	if setting != "true" && setting != "false" {
		s.logger.Warn(fmt.Sprintf("Invalid scooter.battery-ignores-seatbox value: %q (must be 'true' or 'false')", setting))
		return
	}

	enabled := (setting == "true")
	s.config.DangerouslyIgnoreSeatbox = enabled
	s.logger.Info(fmt.Sprintf("Loaded scooter.battery-ignores-seatbox setting: %t", enabled))
}

func (s *Service) handleIgnoreSeatboxSettingChange() {
	setting, err := s.ipc.HGet("settings", "scooter.battery-ignores-seatbox")
	if err != nil {
		s.logger.Error(fmt.Sprintf("Failed to fetch scooter.battery-ignores-seatbox setting: %v", err))
		return
	}

	if setting != "true" && setting != "false" {
		s.logger.Warn(fmt.Sprintf("Invalid scooter.battery-ignores-seatbox value: %q (must be 'true' or 'false')", setting))
		return
	}

	enabled := (setting == "true")
	oldValue := s.config.DangerouslyIgnoreSeatbox
	s.config.DangerouslyIgnoreSeatbox = enabled

	s.logger.Info(fmt.Sprintf("Scooter battery-ignores-seatbox setting changed: %t -> %t", oldValue, enabled))

	// Notify all active readers to re-evaluate their enabled state
	for _, reader := range s.readers {
		if reader != nil && reader.role == BatteryRoleActive {
			reader.triggerRestart()
		}
	}
}

func (s *Service) loadMaxVoltageDeltaSetting() {
	setting, err := s.ipc.HGet("settings", "scooter.max-voltage-delta")
	if err != nil {
		if err != ipc.ErrNil {
			s.logger.Warn(fmt.Sprintf("Failed to load scooter.max-voltage-delta setting: %v", err))
		}
		return
	}

	value, err := strconv.ParseUint(setting, 10, 64)
	if err != nil {
		s.logger.Warn(fmt.Sprintf("Invalid scooter.max-voltage-delta value: %q (must be a number in mV, 0 to disable)", setting))
		return
	}

	s.config.MaxVoltageDelta = value
	s.logger.Info(fmt.Sprintf("Loaded scooter.max-voltage-delta setting: %d mV", value))
}

func (s *Service) handleMaxVoltageDeltaSettingChange() {
	setting, err := s.ipc.HGet("settings", "scooter.max-voltage-delta")
	if err != nil {
		s.logger.Error(fmt.Sprintf("Failed to fetch scooter.max-voltage-delta setting: %v", err))
		return
	}

	value, err := strconv.ParseUint(setting, 10, 64)
	if err != nil {
		s.logger.Warn(fmt.Sprintf("Invalid scooter.max-voltage-delta value: %q (must be a number in mV, 0 to disable)", setting))
		return
	}

	oldValue := s.config.MaxVoltageDelta
	s.config.MaxVoltageDelta = value
	s.logger.Info(fmt.Sprintf("Scooter max-voltage-delta setting changed: %d mV -> %d mV", oldValue, value))
}

func (s *Service) checkVoltageDelta() (ok bool, delta uint64) {
	maxDelta := s.config.MaxVoltageDelta
	if maxDelta == 0 {
		return true, 0
	}

	v0str, err := s.ipc.HGet("battery:0", "voltage")
	if err != nil {
		return true, 0
	}
	v0, err := strconv.ParseUint(v0str, 10, 64)
	if err != nil || v0 == 0 {
		return true, 0
	}

	v1str, err := s.ipc.HGet("battery:1", "voltage")
	if err != nil {
		return true, 0
	}
	v1, err := strconv.ParseUint(v1str, 10, 64)
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
	setting, err := s.ipc.HGet("settings", "scooter.dual-battery")
	if err != nil {
		if err != ipc.ErrNil {
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
				s.logger.Warn(fmt.Sprintf("Voltage delta too large (%dmV > %dmV) - refusing to activate battery 1", delta, s.config.MaxVoltageDelta))
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
	setting, err := s.ipc.HGet("settings", "scooter.dual-battery")
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
			s.logger.Warn(fmt.Sprintf("Voltage delta too large (%dmV > %dmV) - refusing to activate battery 1", delta, s.config.MaxVoltageDelta))
			return
		}
		reader.voltageDeltaBlocked = false
	}

	reader.role = newRole
	s.batteryConfig.Readers[1].Role = newRole

	s.logger.Info(fmt.Sprintf("Scooter dual-battery setting changed: %v -> %v", oldRole, newRole))

	// Update enabled state based on new role
	if newRole == BatteryRoleInactive {
		reader.SetEnabled(false)
	} else {
		if s.config.DangerouslyIgnoreSeatbox {
			reader.SetEnabled(true)
		} else {
			seatboxLock, err := s.ipc.HGet("vehicle", "seatbox:lock")
			if err == nil {
				closed := (seatboxLock == "closed")
				reader.SetEnabled(closed)
			}
		}
	}

	reader.triggerRestart()
}
