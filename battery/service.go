package battery

import (
	"context"
	"fmt"
	"log"
	"time"

	"battery-service/nfc/hal"

	"github.com/redis/go-redis/v9"
)

// NewService creates a new battery service with battery configuration
func NewService(config *ServiceConfig, batteryConfig *BatteryConfiguration, logger *log.Logger, debugMode bool) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Service{
		config:        config,
		batteryConfig: batteryConfig,
		logger:        logger,
		ctx:           ctx,
		cancel:        cancel,
		debug:         debugMode, // Store debugMode
		// Initialize new fields
		vehicleState:  "", // Will be fetched
		readers:       make([]*BatteryReader, len(batteryConfig.Readers)),
	}

	// Initialize Redis client
	s.redis = redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", config.RedisServerAddress, config.RedisServerPort),
	})

	// Test Redis connection
	if err := s.redis.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %v", err)
	}

	// Create battery readers based on configuration
	for i, readerConfig := range batteryConfig.Readers {
		if !readerConfig.Enabled {
			s.logger.Printf("Reader %d is disabled in configuration", readerConfig.Index)
			continue
		}
		
		reader, err := s.createReader(readerConfig.Index, readerConfig.Role, readerConfig.DeviceName, readerConfig.LogLevel)
		if err != nil {
			s.logger.Printf("Failed to create reader %d: %v", readerConfig.Index, err)
			continue
		}
		s.readers[i] = reader
	}

	return s, nil
}

// createReader creates a new battery reader instance
func (s *Service) createReader(index int, role BatteryRole, deviceName string, logLevel int) (*BatteryReader, error) {
	reader := &BatteryReader{
		index:                   index,
		role:                    role,
		deviceName:              deviceName,
		logLevel:                logLevel,
		service:                 s,
		stopChan:                make(chan struct{}),
		lastReinitialization:    time.Now(), // Initialize with current time
		successSignal:           make(chan struct{}, 10), // Initialize with buffer for non-blocking sends
	}

	// Create NFC HAL
	var err error
	reader.hal, err = hal.NewPN7150(deviceName, reader.logCallback, nil, true, false, s.debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create NFC HAL: %v", err)
	}

	// Initialize state machine
	reader.stateMachine = NewBatteryStateMachine(reader)

	return reader, nil
}

// Start starts the battery service
func (s *Service) Start() error {
	// Start Redis subscription
	go s.handleRedisSubscription()

	for _, reader := range s.readers {
		if reader != nil {
			if err := reader.Start(); err != nil {
				s.logger.Printf("Failed to start reader %d: %v", reader.index, err)
			}
		}
	}
	return nil
}

// Stop stops the battery service
func (s *Service) Stop() {
	s.cancel() // Cancel context to stop Redis subscription


	if s.redis != nil {
		if err := s.redis.Close(); err != nil {
			s.logger.Printf("Error closing Redis connection: %v", err)
		}
	}

	for _, reader := range s.readers {
		if reader != nil {
			reader.Stop()
		}
	}
}

// SetEnabled enables or disables a battery reader
func (s *Service) SetEnabled(index int, enabled bool) {
	for _, reader := range s.readers {
		if reader != nil && reader.index == index {
			reader.setEnabled(enabled)
			break
		}
	}
}

// logCallback handles logging for the battery reader
func (r *BatteryReader) logCallback(level hal.LogLevel, message string) {
	r.service.logger.Printf("[Battery %d] %s", r.index, message)
}
