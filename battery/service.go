package battery

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
)

func NewService(config *ServiceConfig, batteryConfig *BatteryConfiguration, logger *log.Logger, debugMode bool) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Service{
		config:        config,
		batteryConfig: batteryConfig,
		logger:        logger,
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
			s.logger.Printf("Reader %d is disabled in configuration", readerConfig.Index)
			continue
		}

		reader, err := NewBatteryReader(readerConfig.Index, readerConfig.Role, readerConfig.DeviceName, readerConfig.LogLevel, s)
		if err != nil {
			s.logger.Printf("Failed to create reader %d: %v", readerConfig.Index, err)
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
	s.logger.Printf("Starting battery service v2")

	go s.runRedisSubscriber()

	for _, reader := range s.readers {
		if reader != nil {
			if err := reader.Start(); err != nil {
				s.logger.Printf("Failed to start reader %d: %v", reader.index, err)
				continue
			}
		}
	}

	s.logger.Printf("Battery service v2 started successfully")
	return nil
}

func (s *Service) Stop() {
	s.logger.Printf("Stopping battery service v2")

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

	s.logger.Printf("Battery service v2 stopped")
}

func (s *Service) SetBatteryEnabled(index int, enabled bool) error {
	if index >= len(s.readers) || s.readers[index] == nil {
		return fmt.Errorf("invalid battery index: %d", index)
	}

	s.readers[index].SetEnabled(enabled)
	return nil
}

func (s *Service) runRedisSubscriber() {
	s.logger.Printf("Starting Redis subscriber for channels: vehicle:state, vehicle:seatbox:lock")

	pubsub := s.redis.Subscribe(s.ctx,
		"vehicle",
	)
	defer pubsub.Close()

	_, err := pubsub.Receive(s.ctx)
	if err != nil {
		s.logger.Printf("ERROR: Failed to establish Redis subscription: %v", err)
		return
	}
	s.logger.Printf("Redis subscription established successfully")

	ch := pubsub.Channel()
	s.logger.Printf("Listening for Redis messages...")

	for {
		select {
		case msg := <-ch:
			if msg == nil {
				s.logger.Printf("Redis channel closed")
				return
			}
			s.logger.Printf("Received Redis message: channel=%s, payload=%s", msg.Channel, msg.Payload)

			switch msg.Channel {
			case "vehicle":
				switch msg.Payload {
				case "state":
					s.handleVehicleStateMessage()
				case "seatbox:lock":
					s.handleSeatboxUpdate()
				default:
					s.logger.Printf("Unknown vehicle payload: %s", msg.Payload)
				}
			default:
				s.logger.Printf("Unknown Redis channel: %s", msg.Channel)
			}

		case <-s.ctx.Done():
			s.logger.Printf("Redis subscriber context cancelled")
			return
		}
	}
}

func (s *Service) handleVehicleStateMessage() {
	vehicleState, err := s.redis.HGet(s.ctx, "vehicle", "state").Result()
	if err != nil {
		s.logger.Printf("Failed to fetch vehicle state: %v", err)
		return
	}

	newState := VehicleState(vehicleState)
	s.logger.Printf("Vehicle state changed: %s", newState)

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
		s.logger.Printf("Failed to fetch seatbox lock state: %v", err)
		return
	}

	closed := (seatboxLock == "closed")
	s.logger.Printf("Seatbox lock changed: %s (closed=%t)", seatboxLock, closed)

	for _, reader := range s.readers {
		if reader != nil {
			reader.SendSeatboxLockChange(closed)
		}
	}
}
