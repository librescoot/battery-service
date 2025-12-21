package battery

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"sync"

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

	s.loadIgnoreSeatboxSetting()
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
				if msg.Payload == "battery.ignore-seatbox" {
					s.handleIgnoreSeatboxSettingChange()
				} else if msg.Payload == "scooter.dual-battery" {
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

func (s *Service) loadIgnoreSeatboxSetting() {
	setting, err := s.redis.HGet(s.ctx, "settings", "battery.ignore-seatbox").Result()
	if err != nil {
		if err != redis.Nil {
			s.logger.Warn(fmt.Sprintf("Failed to load battery.ignore-seatbox setting: %v", err))
		}
		return
	}

	if setting != "true" && setting != "false" {
		s.logger.Warn(fmt.Sprintf("Invalid battery.ignore-seatbox value: %q (must be 'true' or 'false')", setting))
		return
	}

	enabled := (setting == "true")
	s.config.DangerouslyIgnoreSeatbox = enabled
	s.logger.Info(fmt.Sprintf("Loaded battery.ignore-seatbox setting: %t", enabled))
}

func (s *Service) handleIgnoreSeatboxSettingChange() {
	setting, err := s.redis.HGet(s.ctx, "settings", "battery.ignore-seatbox").Result()
	if err != nil {
		s.logger.Error(fmt.Sprintf("Failed to fetch battery.ignore-seatbox setting: %v", err))
		return
	}

	if setting != "true" && setting != "false" {
		s.logger.Warn(fmt.Sprintf("Invalid battery.ignore-seatbox value: %q (must be 'true' or 'false')", setting))
		return
	}

	enabled := (setting == "true")
	oldValue := s.config.DangerouslyIgnoreSeatbox
	s.config.DangerouslyIgnoreSeatbox = enabled

	s.logger.Info(fmt.Sprintf("Battery ignore-seatbox setting changed: %t -> %t", oldValue, enabled))

	// Notify all active readers to re-evaluate their enabled state
	for _, reader := range s.readers {
		if reader != nil && reader.role == BatteryRoleActive {
			// Trigger a restart to apply the new seatbox ignore setting
			reader.triggerRestart()
		}
	}
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
		if dualBattery {
			s.batteryConfig.Readers[1].Role = BatteryRoleActive
		} else {
			s.batteryConfig.Readers[1].Role = BatteryRoleInactive
		}
		s.logger.Info(fmt.Sprintf("Loaded scooter.dual-battery setting: %t (battery 1 role: %v)", dualBattery, s.batteryConfig.Readers[1].Role))
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

	reader.role = newRole
	s.batteryConfig.Readers[1].Role = newRole

	s.logger.Info(fmt.Sprintf("Scooter dual-battery setting changed: %v -> %v", oldRole, newRole))

	// Update enabled state based on new role
	if newRole == BatteryRoleInactive {
		// Inactive batteries are always disabled
		reader.SetEnabled(false)
	} else {
		// Active batteries follow seatbox state (unless ignoring seatbox)
		if s.config.DangerouslyIgnoreSeatbox {
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
