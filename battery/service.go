package battery

import (
	"context"
	"fmt"
	"log"
	"log/slog"
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

	ipcClient, err := ipc.New(
		ipc.WithAddress(config.RedisServerAddress),
		ipc.WithPort(int(config.RedisServerPort)),
		ipc.WithCodec(ipc.StringCodec{}), // Use plain strings, not JSON (matches existing IPC)
		ipc.WithOnConnect(func() { s.logger.Info("Redis connected") }),
		ipc.WithOnDisconnect(func(err error) {
			if err != nil {
				s.logger.Error(fmt.Sprintf("Redis disconnected: %v", err))
			}
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %v", err)
	}

	s.ipcClient = ipcClient

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

	s.setupWatchers()

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

	if s.ipcClient != nil {
		s.ipcClient.Close()
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

func (s *Service) setupWatchers() {
	s.logger.Info("Setting up Redis watchers for vehicle and settings")

	// Vehicle watcher for state and seatbox:lock
	s.vehicleWatcher = s.ipcClient.NewHashWatcher("vehicle")
	s.vehicleWatcher.OnField("state", func(value string) error {
		s.logger.Debug(fmt.Sprintf("Received vehicle state update: %s", value))
		s.handleVehicleState(value)
		return nil
	})
	s.vehicleWatcher.OnField("seatbox:lock", func(value string) error {
		s.logger.Debug(fmt.Sprintf("Received seatbox lock update: %s", value))
		s.handleSeatboxLock(value)
		return nil
	})

	if err := s.vehicleWatcher.StartWithSync(); err != nil {
		s.logger.Error(fmt.Sprintf("Failed to start vehicle watcher: %v", err))
		s.stdLogger.Fatal("Redis connection failed, exiting to allow systemd restart")
	}

	// Settings watcher for battery-ignores-seatbox and dual-battery
	s.settingsWatcher = s.ipcClient.NewHashWatcher("settings")
	s.settingsWatcher.OnField("scooter.battery-ignores-seatbox", func(value string) error {
		s.logger.Debug(fmt.Sprintf("Received battery-ignores-seatbox update: %s", value))
		s.handleIgnoreSeatboxSetting(value)
		return nil
	})
	s.settingsWatcher.OnField("scooter.dual-battery", func(value string) error {
		s.logger.Debug(fmt.Sprintf("Received dual-battery update: %s", value))
		s.handleDualBatterySetting(value)
		return nil
	})

	if err := s.settingsWatcher.StartWithSync(); err != nil {
		s.logger.Error(fmt.Sprintf("Failed to start settings watcher: %v", err))
		s.stdLogger.Fatal("Redis connection failed, exiting to allow systemd restart")
	}

	s.logger.Info("Redis watchers established successfully")
}

func (s *Service) handleVehicleState(value string) {
	newState := VehicleState(value)
	s.logger.Info(fmt.Sprintf("Vehicle state changed: %s", newState))

	s.vehicleState = newState

	for _, reader := range s.readers {
		if reader != nil {
			reader.SendVehicleStateChange(newState)
		}
	}
}

func (s *Service) handleSeatboxLock(value string) {
	closed := (value == "closed")
	s.logger.Info(fmt.Sprintf("Seatbox lock changed: %s (closed=%t)", value, closed))

	for _, reader := range s.readers {
		if reader != nil {
			reader.SendSeatboxLockChange(closed)
		}
	}
}

func (s *Service) handleIgnoreSeatboxSetting(value string) {
	if value != "true" && value != "false" {
		s.logger.Warn(fmt.Sprintf("Invalid scooter.battery-ignores-seatbox value: %q (must be 'true' or 'false')", value))
		return
	}

	enabled := (value == "true")
	oldValue := s.config.DangerouslyIgnoreSeatbox
	s.config.DangerouslyIgnoreSeatbox = enabled

	s.logger.Info(fmt.Sprintf("Scooter battery-ignores-seatbox setting: %t (changed: %t)", enabled, oldValue != enabled))

	// Notify all active readers to re-evaluate their enabled state (only if changed)
	if oldValue != enabled {
		for _, reader := range s.readers {
			if reader != nil && reader.role == BatteryRoleActive {
				// Trigger a restart to apply the new seatbox ignore setting
				reader.triggerRestart()
			}
		}
	}
}

func (s *Service) handleDualBatterySetting(value string) {
	if value != "true" && value != "false" {
		s.logger.Warn(fmt.Sprintf("Invalid scooter.dual-battery value: %q (must be 'true' or 'false')", value))
		return
	}

	dualBattery := (value == "true")

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
			// Fetch current seatbox state
			seatboxLock, err := s.ipcClient.HGet("vehicle", "seatbox:lock")
			if err == nil {
				closed := (seatboxLock == "closed")
				reader.SetEnabled(closed)
			}
		}
	}

	// Trigger restart to apply the new role
	reader.triggerRestart()
}
