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
	s.logger.Info("Starting Redis subscriber for channels: vehicle:state, vehicle:seatbox:lock")

	pubsub := s.redis.Subscribe(s.ctx,
		"vehicle",
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
