package battery

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"battery-service/nfc/hal"

	"github.com/redis/go-redis/v9"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const (
	// Memory addresses for battery data
	addrStatus0 = 0x0300
	addrStatus1 = 0x0310
	addrStatus2 = 0x0320
	addrCommand = 0x0330
	addrConfig  = 0x03A0
	addrSession = 0x03B0

	// Timing constants
	timeHeartbeatOn     = 5 * time.Second
	timeCmd             = 200 * time.Millisecond
	timeCmdSlow         = 800 * time.Millisecond
	timeCmdFirstOpened  = 500 * time.Millisecond
	timeCmdFirstAsleep  = 500 * time.Millisecond
	timePresence        = 10 * time.Second
	timeCheckReader     = 10 * time.Second
	timeDeparture      = 500 * time.Millisecond
	timeBattery1Poll    = 10 * time.Minute  // Special polling interval for battery 1
	timeStateVerify     = 1000 * time.Millisecond // Time to wait before verifying state change
	timeActivationRetry = 2 * time.Second   // Time between activation retry attempts

	// Constants for temperature limits
	temperatureStateColdLimit = -10 // Celsius
	temperatureStateHotLimit  = 60  // Celsius

	// Retry constants
	maxReadRetries     = 2
	maxWriteRetries    = 2
	maxActivationRetries = 3 // Maximum number of times to retry activation sequence
)

// BatteryReader represents a single battery reader instance
type BatteryReader struct {
	sync.Mutex
	index     int
	hal       hal.HAL
	data      BatteryData
	enabled   bool
	config    *BatteryConfig
	service   *Service
	lastCmd   time.Time
	justInserted bool
	justOpened   bool
	stopChan     chan struct{} // Channel to signal goroutine shutdown
}

// Service represents the battery service that manages multiple readers
type Service struct {
	sync.Mutex
	config   *ServiceConfig
	readers  [2]*BatteryReader
	logger   *log.Logger
	redis    *redis.Client
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewService creates a new battery service
func NewService(config *ServiceConfig, logger *log.Logger) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())
	
	s := &Service{
		config: config,
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	}

	// Initialize Redis client
	s.redis = redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", config.RedisServerAddress, config.RedisServerPort),
	})

	// Test Redis connection
	if err := s.redis.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %v", err)
	}

	// Create battery readers
	for i := 0; i < 2; i++ {
		reader, err := s.createReader(i, &config.Batteries[i])
		if err != nil {
			s.logger.Printf("Failed to create reader %d: %v", i, err)
			continue
		}
		s.readers[i] = reader
	}

	return s, nil
}

// createReader creates a new battery reader instance
func (s *Service) createReader(index int, config *BatteryConfig) (*BatteryReader, error) {
	reader := &BatteryReader{
		index:   index,
		config:  config,
		service: s,
		stopChan: make(chan struct{}),
	}

	// Create NFC HAL
	var err error
	reader.hal, err = hal.NewPN7150(config.DeviceName, reader.logCallback, nil, true, false)
	if err != nil {
		return nil, fmt.Errorf("failed to create NFC HAL: %v", err)
	}

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
	if reader := s.readers[index]; reader != nil {
		reader.setEnabled(enabled)
	}
}

// Start starts the battery reader
func (r *BatteryReader) Start() error {
	// Enable the reader by default
	r.enabled = true

	// Initialize NFC HAL
	if err := r.hal.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize NFC HAL: %v", err)
	}

	// Start tag monitoring
	go r.monitorTags()

	// Start heartbeat
	r.startHeartbeat()

	return nil
}

// Stop stops the battery reader
func (r *BatteryReader) Stop() {
	close(r.stopChan)
	r.hal.Deinitialize()
}

// setEnabled enables or disables the battery reader
func (r *BatteryReader) setEnabled(enabled bool) {
	r.Lock()
	defer r.Unlock()
	r.enabled = enabled
}

// monitorTags continuously monitors for tag presence
func (r *BatteryReader) monitorTags() {
	var lastBattery1Poll time.Time // Track last poll time for battery 1
	
	// Initialize battery state as not present
	r.Lock()
	if !r.data.Present {
		r.data = BatteryData{} // Ensure clean state
		if err := r.updateRedisStatus(); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to initialize Redis status: %v", err))
		}
	}
	r.Unlock()
	
	for {
		select {
		case <-r.stopChan:
			return
		default:
			// For battery 1, only poll every 10 minutes
			if r.index == 1 && r.data.Present {
				if time.Since(lastBattery1Poll) < timeBattery1Poll {
					time.Sleep(time.Second) // Sleep for a second before checking again
					continue
				}
				lastBattery1Poll = time.Now()
			}

			tags, err := r.hal.DetectTags()
			if err != nil {
				r.logCallback(hal.LogLevelError, fmt.Sprintf("Tag detection error: %v", err))
				time.Sleep(timeDeparture)
				continue
			}

			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Detected %d tags", len(tags)))

			if len(tags) == 1 && (tags[0].RFProtocol == hal.RFProtocolT2T || tags[0].RFProtocol == hal.RFProtocolISODEP) {
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Found %s tag with ID: %x", tags[0].RFProtocol, tags[0].ID))
				r.handleTagPresent()
			} else if len(tags) > 1 {
				r.logCallback(hal.LogLevelWarning, "Multiple tags detected, ignoring")
				r.handleTagAbsent()
			} else {
				r.handleTagAbsent()
			}

			// Sleep interval based on battery index and presence
			if r.index == 1 {
				if r.data.Present {
					// For battery 1 when present, use the long poll interval
					time.Sleep(timeBattery1Poll)
				} else {
					// For battery 1 when not present, use a moderate poll interval
					time.Sleep(5 * time.Second)
				}
			} else {
				// For battery 0, use the standard short poll interval
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
}

// calculateTemperatureState determines the battery temperature state
func (r *BatteryReader) calculateTemperatureState() BatteryTemperatureState {
	for _, temp := range r.data.Temperature {
		if temp <= temperatureStateColdLimit {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("cold, temperature=%d", temp))
			return BatteryTemperatureStateCold
		}
		if temp >= temperatureStateHotLimit {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("hot, temperature=%d", temp))
			return BatteryTemperatureStateHot
		}
	}
	return BatteryTemperatureStateIdeal
}

// startHeartbeat starts the heartbeat goroutine
func (r *BatteryReader) startHeartbeat() {
	go func() {
		ticker := time.NewTicker(timeHeartbeatOn)
		defer ticker.Stop()

		for {
			select {
			case <-r.stopChan:
				return
			case <-ticker.C:
				r.Lock()
				if r.data.Present {
					if r.data.State == BatteryStateActive {
						r.logCallback(hal.LogLevelDebug, "Sending heartbeat command")
						r.sendCommand(BatteryCommandHeartbeatScooter)
					} else if r.enabled && !r.data.LowSOC {
						// Actively try to correct battery state if not ACTIVE
						r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery not active during heartbeat (state=%s), attempting activation", r.data.State))
						r.Unlock() // Unlock before calling updateBatteryState to avoid deadlock
						r.activateBattery()
						r.Lock() // Re-acquire lock
					} else {
						r.logCallback(hal.LogLevelDebug, "Battery present, continuing heartbeat")
					}
				}
				r.Unlock()
			}
		}
	}()
}

// handleTagPresent handles a present tag
func (r *BatteryReader) handleTagPresent() {
	r.Lock()
	defer r.Unlock()

	if !r.data.Present {
		r.data.Present = true // Mark as present immediately upon detection
		r.justInserted = true
		// We'll log the battery info after reading status
	}

	if err := r.readBatteryStatus(); err != nil {
		r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to read battery status: %v", err))
		r.data.EmptyOr0Data++
		return
	}

	if r.justInserted {
		r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery inserted: serial_number=%s, fw_version=%s, charge=%d, state=%s",
			string(r.data.SerialNumber[:]), r.data.FWVersion, r.data.Charge, r.data.State))
		
		// Send INSERTED command only once when battery is first detected
		r.logCallback(hal.LogLevelDebug, "Sending inserted command for new battery")
		r.sendCommand(BatteryCommandInsertedInScooter)
		r.justInserted = false
	}

	// Calculate temperature state
	oldTempState := r.data.TemperatureState
	r.data.TemperatureState = r.calculateTemperatureState()
	if oldTempState != r.data.TemperatureState {
		r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery temperature-state: %s", r.data.TemperatureState))
	}

	// Remove duplicate INSERTED command send from here
	r.updateBatteryState()
}

// handleTagAbsent handles an absent tag
func (r *BatteryReader) handleTagAbsent() {
	r.Lock()
	defer r.Unlock()

	if r.data.Present {
		r.logCallback(hal.LogLevelInfo, "Battery removed")
		r.data = BatteryData{} // Clear data
		r.justInserted = false
		r.justOpened = false
		
		// Update Redis to reflect the battery is no longer present
		if err := r.updateRedisStatus(); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after battery removal: %v", err))
		}
	}
}

// readRegisterWithRetry reads a register with retries and verification
func (r *BatteryReader) readRegisterWithRetry(addr uint16) ([]byte, error) {
	var lastErr error
	var data, checkData []byte
	var verified bool
	var successfulReads int

	for retry := 0; retry < maxReadRetries; retry++ {
		// First read attempt
		data, lastErr = r.hal.ReadBinary(addr)
		if lastErr != nil {
			// Check if we lost connection
			if r.hal.GetState() != hal.StatePresent {
				lastErr = fmt.Errorf("lost connection during read: %v", lastErr)
				// Add a longer delay and attempt to recover the connection
				time.Sleep(timeCmdFirstOpened)
				if r.hal.GetState() == hal.StateDiscovering {
					// Wait for potential rediscovery
					time.Sleep(timeCmdFirstAsleep)
				}
				continue
			}
			// For other errors, add a shorter delay
			time.Sleep(timeCmd)
			continue
		}

		// Check for 0300 response which indicates we should go back to discovering
		if len(data) == 2 && data[0] == 0x03 && data[1] == 0x00 {
			r.logCallback(hal.LogLevelDebug, "Received 0300 response, transitioning to discovering state")
			// Force state transition by returning error
			return nil, fmt.Errorf("received 0300 response, transitioning to discovering state")
		}

		// Check for truncated response
		if len(data) < 16 {
			// If we get a truncated response, add a longer delay
			lastErr = fmt.Errorf("truncated response: got %d bytes, expected 16", len(data))
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmdSlow) // Use slower delay for truncated responses
			continue
		}

		// If we get here, we have a full response
		// Add a small delay before verification read to let the tag stabilize
		time.Sleep(timeCmd)

		// Verify state is still good before verification read
		if r.hal.GetState() != hal.StatePresent {
			lastErr = fmt.Errorf("lost connection before verification read")
			time.Sleep(timeCmdSlow)
			continue
		}

		// Verification read
		checkData, lastErr = r.hal.ReadBinary(addr)
		if lastErr != nil {
			// Check if we lost connection
			if r.hal.GetState() != hal.StatePresent {
				lastErr = fmt.Errorf("lost connection during verification read: %v", lastErr)
				time.Sleep(timeCmdSlow)
				continue
			}
			// For other errors, add a shorter delay
			time.Sleep(timeCmd)
			continue
		}

		// Check for truncated verification response
		if len(checkData) < 16 {
			lastErr = fmt.Errorf("truncated verification response: got %d bytes, expected 16", len(checkData))
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmd)
			continue
		}

		// Track successful reads even if they don't match
		successfulReads++

		// Find the first non-FF byte in both responses
		firstNonFFData := -1
		firstNonFFCheck := -1
		for i := 0; i < min(len(data), len(checkData)); i++ {
			if firstNonFFData == -1 && data[i] != 0xFF {
				firstNonFFData = i
			}
			if firstNonFFCheck == -1 && checkData[i] != 0xFF {
				firstNonFFCheck = i
			}
			if firstNonFFData != -1 && firstNonFFCheck != -1 {
				break
			}
		}

		// If either response is all FFs, try again
		if firstNonFFData == -1 || firstNonFFCheck == -1 {
			lastErr = fmt.Errorf("received all FF response")
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmdSlow)
			continue
		}

		// Compare the non-FF parts
		if bytes.Equal(data[firstNonFFData:], checkData[firstNonFFCheck:]) {
			verified = true
			// Keep the data starting from first non-FF byte
			data = data[firstNonFFData:]
			break
		}

		// If both responses start with the same meaningful data but differ only in trailing FFs,
		// consider them matching
		meaningfulLen := 0
		for i := 0; i < min(len(data), len(checkData)); i++ {
			if data[i] == 0xFF && checkData[i] == 0xFF {
				break
			}
			if data[i] != checkData[i] {
				meaningfulLen = 0
				break
			}
			meaningfulLen = i + 1
		}
		if meaningfulLen > 0 {
			verified = true
			data = data[:meaningfulLen]
			break
		}

		// If we've had multiple successful reads but they don't match,
		// keep the longer response if one is truncated
		if successfulReads >= 2 {
			if len(data) == 16 && len(checkData) < 16 {
				verified = true
				break
			}
			if len(checkData) == 16 && len(data) < 16 {
				data = checkData
				verified = true
				break
			}
		}

		// Data mismatch, log the difference and try again
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Data mismatch at 0x%04x: %X != %X", addr, data, checkData))
		lastErr = fmt.Errorf("data verification failed")
		time.Sleep(timeCmd)
	}

	if !verified {
		return nil, fmt.Errorf("failed to read register 0x%04x after %d retries: %v", addr, maxReadRetries, lastErr)
	}

	// Check for zero data
	allZero := true
	for _, b := range data {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("received zero data at register 0x%04x", addr))
		r.data.EmptyOr0Data++
		return nil, fmt.Errorf("received zero data at register 0x%04x", addr)
	}

	return data, nil
}

// updateRedisStatus updates the Redis hash with current battery status
func (r *BatteryReader) updateRedisStatus() error {
	if r.service.redis == nil {
		return fmt.Errorf("Redis client not initialized")
	}

	// Create a map for the battery status fields
	status := map[string]interface{}{
		"present":            fmt.Sprintf("%v", r.data.Present),
		"state":             r.data.State.String(),
		"voltage":           fmt.Sprintf("%d", r.data.Voltage),
		"current":           fmt.Sprintf("%d", r.data.Current),
		"charge":            fmt.Sprintf("%d", r.data.Charge),
		"temperature:0":     fmt.Sprintf("%d", r.data.Temperature[0]),
		"temperature:1":     fmt.Sprintf("%d", r.data.Temperature[1]),
		"temperature:2":     fmt.Sprintf("%d", r.data.Temperature[2]),
		"temperature:3":     fmt.Sprintf("%d", r.data.Temperature[3]),
		"temperature-state": r.data.TemperatureState.String(),
		"cycle-count":       fmt.Sprintf("%d", r.data.CycleCount),
		"state-of-health":   fmt.Sprintf("%d", r.data.StateOfHealth),
		"serial-number":     string(bytes.TrimRight(r.data.SerialNumber[:], "\x00")),
		"manufacturing-date": r.data.ManufacturingDate,
		"fw-version":        r.data.FWVersion,
	}

	// Set all non-fault fields in the hash
	key := fmt.Sprintf("battery:%d", r.index)
	if err := r.service.redis.HMSet(r.service.ctx, key, status).Err(); err != nil {
		return fmt.Errorf("failed to update Redis status: %v", err)
	}

	// Handle fault fields - only write true values, clear false values
	// Define all possible fault fields
	faultFields := []struct {
		field string
		value bool
	}{
		{"faults:charge-temp-over-high", r.data.Faults.ChargeTempOverHigh},
		{"faults:charge-temp-over-low", r.data.Faults.ChargeTempOverLow},
		{"faults:discharge-temp-over-high", r.data.Faults.DischargeTempOverHigh},
		{"faults:discharge-temp-over-low", r.data.Faults.DischargeTempOverLow},
		{"faults:signal-wire-broken", r.data.Faults.SignalWireBroken},
		{"faults:second-level-over-temp", r.data.Faults.SecondLevelOverTemp},
		{"faults:pack-voltage-high", r.data.Faults.PackVoltageHigh},
		{"faults:mos-temp-over-high", r.data.Faults.MOSTempOverHigh},
		{"faults:cell-voltage-high", r.data.Faults.CellVoltageHigh},
		{"faults:pack-voltage-low", r.data.Faults.PackVoltageLow},
		{"faults:cell-voltage-low", r.data.Faults.CellVoltageLow},
		{"faults:charge-over-current", r.data.Faults.ChargeOverCurrent},
		{"faults:discharge-over-current", r.data.Faults.DischargeOverCurrent},
		{"faults:short-circuit", r.data.Faults.ShortCircuit},
		{"faults:not-following-command", r.data.Faults.NotFollowingCommand},
		{"faults:zero-data", r.data.Faults.ZeroData},
		{"faults:communication-error", r.data.Faults.CommunicationError},
		{"faults:reader-error", r.data.Faults.ReaderError},
	}

	// Process each fault field
	for _, fault := range faultFields {
		if fault.value {
			// If fault is true, set it in Redis
			if err := r.service.redis.HSet(r.service.ctx, key, fault.field, "true").Err(); err != nil {
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to set fault %s: %v", fault.field, err))
			}
		} else {
			// If fault is false, check if it exists in Redis and delete it if it does
			exists, err := r.service.redis.HExists(r.service.ctx, key, fault.field).Result()
			if err != nil {
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to check if fault %s exists: %v", fault.field, err))
				continue
			}
			
			if exists {
				// Delete the fault field if it exists but is now false
				if err := r.service.redis.HDel(r.service.ctx, key, fault.field).Err(); err != nil {
					r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to clear fault %s: %v", fault.field, err))
				} else {
					r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Cleared fault %s", fault.field))
				}
			}
		}
	}

	r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Updated Redis status for battery:%d", r.index))
	return nil
}

// readBatteryStatus reads the battery status registers
func (r *BatteryReader) readBatteryStatus() error {
	r.logCallback(hal.LogLevelDebug, "Reading battery status...")

	// First ensure we're in the correct state for reading
	if r.hal.GetState() != hal.StatePresent {
		return fmt.Errorf("invalid state for reading: %s", r.hal.GetState())
	}

	// Read Status0 with proper state handling
	r.logCallback(hal.LogLevelDebug, "Reading Status0...")
	var data []byte
	var err error
	var retryCount int

	for retryCount = 0; retryCount < maxReadRetries; retryCount++ {
		// Verify state before each attempt
		if r.hal.GetState() != hal.StatePresent {
			time.Sleep(timeCmdSlow)
			continue
		}

		data, err = r.readRegisterWithRetry(addrStatus0)
		if err == nil {
			break
		}
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: Failed to read Status0: %v", retryCount+1, err))
		time.Sleep(timeCmdSlow) // Add delay between retries
	}

	if err != nil {
		return fmt.Errorf("failed to read Status0 after %d retries: %v", maxReadRetries, err)
	}

	// Process Status0 data
	r.data.Present = true
	r.data.Voltage = uint16(data[1])<<8 | uint16(data[0])
	r.data.Current = int16(uint16(data[3])<<8 | uint16(data[2]))
	r.data.FWVersion = fmt.Sprintf("%d.%d", data[4], data[5])
	r.data.RemainingCapacity = uint16(data[7])<<8 | uint16(data[6])
	r.data.FullCapacity = uint16(data[9])<<8 | uint16(data[8])
	if r.data.FullCapacity > 0 {
		r.data.Charge = uint8((uint32(r.data.RemainingCapacity) * 100) / uint32(r.data.FullCapacity))
	}
	r.data.FaultCode = uint16(data[11])<<8 | uint16(data[10])
	r.data.Temperature[0] = int8(data[12])
	r.data.Temperature[1] = int8(data[13])
	r.data.StateOfHealth = data[14]
	r.data.LowSOC = data[15] != 0

	// Initialize faults struct
	r.data.Faults = BatteryFaults{}

	// Parse fault code bitmask
	faultCode := r.data.FaultCode
	r.data.Faults.ChargeTempOverHigh = (faultCode>>0)&1 != 0
	r.data.Faults.ChargeTempOverLow = (faultCode>>1)&1 != 0
	r.data.Faults.DischargeTempOverHigh = (faultCode>>2)&1 != 0
	r.data.Faults.DischargeTempOverLow = (faultCode>>3)&1 != 0
	r.data.Faults.SignalWireBroken = (faultCode>>4)&1 != 0
	r.data.Faults.SecondLevelOverTemp = (faultCode>>5)&1 != 0
	r.data.Faults.PackVoltageHigh = (faultCode>>6)&1 != 0
	r.data.Faults.MOSTempOverHigh = (faultCode>>7)&1 != 0
	r.data.Faults.CellVoltageHigh = (faultCode>>8)&1 != 0
	r.data.Faults.PackVoltageLow = (faultCode>>9)&1 != 0
	r.data.Faults.CellVoltageLow = (faultCode>>10)&1 != 0
	r.data.Faults.ChargeOverCurrent = (faultCode>>11)&1 != 0
	r.data.Faults.DischargeOverCurrent = (faultCode>>12)&1 != 0
	r.data.Faults.ShortCircuit = (faultCode>>13)&1 != 0
	// Bits 14 and 15 are reserved

	// Log specific faults if present
	if r.data.Faults.ChargeTempOverHigh {
		r.logCallback(hal.LogLevelWarning, "Fault: Charge Temperature Over High")
	}
	if r.data.Faults.ChargeTempOverLow {
		r.logCallback(hal.LogLevelWarning, "Fault: Charge Temperature Over Low")
	}
	if r.data.Faults.DischargeTempOverHigh {
		r.logCallback(hal.LogLevelWarning, "Fault: Discharge Temperature Over High")
	}
	if r.data.Faults.DischargeTempOverLow {
		r.logCallback(hal.LogLevelWarning, "Fault: Discharge Temperature Over Low")
	}
	if r.data.Faults.SignalWireBroken {
		r.logCallback(hal.LogLevelWarning, "Fault: Signal Wire Broken")
	}
	if r.data.Faults.SecondLevelOverTemp {
		r.logCallback(hal.LogLevelWarning, "Fault: Second Level Over Temperature")
	}
	if r.data.Faults.PackVoltageHigh {
		r.logCallback(hal.LogLevelWarning, "Fault: Pack Voltage High")
	}
	if r.data.Faults.MOSTempOverHigh {
		r.logCallback(hal.LogLevelWarning, "Fault: MOS Temperature Over High")
	}
	if r.data.Faults.CellVoltageHigh {
		r.logCallback(hal.LogLevelWarning, "Fault: Cell Voltage High")
	}
	if r.data.Faults.PackVoltageLow {
		r.logCallback(hal.LogLevelWarning, "Fault: Pack Voltage Low")
	}
	if r.data.Faults.CellVoltageLow {
		r.logCallback(hal.LogLevelWarning, "Fault: Cell Voltage Low")
	}
	if r.data.Faults.ChargeOverCurrent {
		r.logCallback(hal.LogLevelWarning, "Fault: Charge Over Current")
	}
	if r.data.Faults.DischargeOverCurrent {
		r.logCallback(hal.LogLevelWarning, "Fault: Discharge Over Current")
	}
	if r.data.Faults.ShortCircuit {
		r.logCallback(hal.LogLevelWarning, "Fault: Short Circuit")
	}
	// Note: Faults like NotFollowingCommand, ZeroData, CommunicationError, and ReaderError
	// are detected and logged elsewhere in the code based on communication outcomes,
	// not directly from the battery's fault code bitmask.

	r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Status0 decoded: voltage=%dmV current=%dmA fw_version=%s remaining_cap=%dmAh full_cap=%dmAh charge=%d%% fault=0x%04x temp1=%d°C temp2=%d°C soh=%d%% low_soc=%v",
		r.data.Voltage, r.data.Current, r.data.FWVersion, r.data.RemainingCapacity, r.data.FullCapacity, r.data.Charge, r.data.FaultCode,
		r.data.Temperature[0], r.data.Temperature[1], r.data.StateOfHealth, r.data.LowSOC))

	// Add delay between register reads and verify state
	time.Sleep(timeCmd)
	if r.hal.GetState() != hal.StatePresent {
		return fmt.Errorf("lost connection after reading Status0")
	}

	// Read Status1 with retries
	r.logCallback(hal.LogLevelDebug, "Reading Status1...")
	for retryCount = 0; retryCount < maxReadRetries; retryCount++ {
		// Verify state before each attempt
		if r.hal.GetState() != hal.StatePresent {
			time.Sleep(timeCmdSlow)
			continue
		}

		data, err = r.readRegisterWithRetry(addrStatus1)
		if err == nil {
			break
		}
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: Failed to read Status1: %v", retryCount+1, err))
		time.Sleep(timeCmdSlow)
	}

	if err != nil {
		return fmt.Errorf("failed to read Status1 after %d retries: %v", maxReadRetries, err)
	}

	// Process Status1 data
	r.data.State = BatteryState(uint32(data[3])<<24 | uint32(data[2])<<16 | uint32(data[1])<<8 | uint32(data[0]))
	copy(r.data.SerialNumber[0:12], data[4:16])

	r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Status1 decoded: state=%s serial_number_part1=%s",
		r.data.State, string(r.data.SerialNumber[0:12])))

	// Add delay between register reads and verify state
	time.Sleep(timeCmd)
	if r.hal.GetState() != hal.StatePresent {
		return fmt.Errorf("lost connection after reading Status1")
	}

	// Read Status2 with retries
	r.logCallback(hal.LogLevelDebug, "Reading Status2...")
	for retryCount = 0; retryCount < maxReadRetries; retryCount++ {
		// Verify state before each attempt
		if r.hal.GetState() != hal.StatePresent {
			time.Sleep(timeCmdSlow)
			continue
		}

		data, err = r.readRegisterWithRetry(addrStatus2)
		if err == nil {
			break
		}
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: Failed to read Status2: %v", retryCount+1, err))
		time.Sleep(timeCmdSlow)
	}

	if err != nil {
		return fmt.Errorf("failed to read Status2 after %d retries: %v", maxReadRetries, err)
	}

	// Process Status2 data
	copy(r.data.SerialNumber[12:16], data[0:4])
	r.data.ManufacturingDate = fmt.Sprintf("%c%c%c%c-%c%c-%c%c",
		data[4], data[5], data[6], data[7],
		data[8], data[9],
		data[10], data[11])
	r.data.CycleCount = uint16(data[13])<<8 | uint16(data[12])
	r.data.Temperature[2] = int8(data[14])
	r.data.Temperature[3] = int8(data[15])

	r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Status2 decoded: serial_number_complete=%s mfg_date=%s cycle_count=%d temp3=%d°C temp4=%d°C",
		string(r.data.SerialNumber[:]), r.data.ManufacturingDate, r.data.CycleCount, r.data.Temperature[2], r.data.Temperature[3]))

	// Update temperature state
	r.data.TemperatureState = r.calculateTemperatureState()

	r.logCallback(hal.LogLevelDebug, "Battery status read complete")

	// Update Redis with the new status
	if err := r.updateRedisStatus(); err != nil {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis: %v", err))
	}

	return nil
}

// activateBattery implements a persistent retry mechanism to activate the battery
func (r *BatteryReader) activateBattery() {
	// For battery 1, never enable it - only poll status
	if r.index == 1 {
		return
	}

	r.Lock()
	defer r.Unlock()

	if !r.enabled {
		if r.data.State != BatteryStateAsleep && r.data.State != BatteryStateIdle {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sending OFF command (disabled): current_state=%s", r.data.State))
			r.sendCommand(BatteryCommandOff)
		}
		return
	}

	if r.data.LowSOC {
		if r.data.State != BatteryStateAsleep && r.data.State != BatteryStateIdle {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sending OFF command (low SOC): current_state=%s", r.data.State))
			r.sendCommand(BatteryCommandOff)
		}
		return
	}

	// If already active, nothing to do
	if r.data.State == BatteryStateActive {
		return
	}

	// Persistent retry loop for activation
	for attempt := 0; attempt < maxActivationRetries; attempt++ {
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Activation attempt %d of %d", attempt+1, maxActivationRetries))
		
		// First send seatbox closed command if we're transitioning from idle/asleep to active
		if r.data.State == BatteryStateIdle || r.data.State == BatteryStateAsleep {
			// Wait for any previous command to settle
			time.Sleep(timeCmdSlow)
			
			// Verify state before sending seatbox closed
			if r.hal.GetState() != hal.StatePresent {
				r.logCallback(hal.LogLevelWarning, "Invalid state before SEATBOX_CLOSED command")
				if attempt < maxActivationRetries-1 {
					time.Sleep(timeActivationRetry)
					continue
				}
				return
			}

			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sending SEATBOX_CLOSED command before ON: current_state=%s", r.data.State))
			r.sendCommand(BatteryCommandSeatboxClosed)
			
			// Wait longer for the seatbox closed command to be processed
			time.Sleep(timeCmdFirstOpened)
			
			// If we lost connection, wait for potential rediscovery
			if r.hal.GetState() == hal.StateDiscovering {
				r.logCallback(hal.LogLevelDebug, "Waiting for rediscovery after SEATBOX_CLOSED")
				time.Sleep(timeCmdFirstAsleep)
				
				// Verify state after waiting
				if r.hal.GetState() != hal.StatePresent {
					r.logCallback(hal.LogLevelWarning, "Failed to recover connection after SEATBOX_CLOSED")
					if attempt < maxActivationRetries-1 {
						time.Sleep(timeActivationRetry)
						continue
					}
					return
				}
			}

			// Re-read battery status to verify state
			if err := r.readBatteryStatus(); err != nil {
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to verify state after SEATBOX_CLOSED: %v", err))
				if attempt < maxActivationRetries-1 {
					time.Sleep(timeActivationRetry)
					continue
				}
				return
			}

			// Only proceed with ON command if we're still in a valid state
			if r.data.State != BatteryStateIdle && r.data.State != BatteryStateAsleep {
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Invalid state after SEATBOX_CLOSED: %s", r.data.State))
				if attempt < maxActivationRetries-1 {
					time.Sleep(timeActivationRetry)
					continue
				}
				return
			}
		}
		
		// Send ON command
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sending ON command: current_state=%s", r.data.State))
		r.sendCommand(BatteryCommandOn)
		
		// Wait for state change to take effect
		time.Sleep(timeStateVerify)
		
		// Re-read battery status to verify state change
		if err := r.readBatteryStatus(); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to verify state after ON command: %v", err))
			if attempt < maxActivationRetries-1 {
				time.Sleep(timeActivationRetry)
				continue
			}
			return
		}
		
		// Check if state changed to ACTIVE
		if r.data.State == BatteryStateActive {
			r.logCallback(hal.LogLevelInfo, "Battery successfully activated")
			// Set NotFollowingCommand fault to false since command was successful
			r.data.Faults.NotFollowingCommand = false
			if err := r.updateRedisStatus(); err != nil {
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after activation: %v", err))
			}
			return
		}
		
		// State didn't change to ACTIVE, set fault and retry
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery not following command: state=%s after ON command", r.data.State))
		r.data.Faults.NotFollowingCommand = true
		if err := r.updateRedisStatus(); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after fault: %v", err))
		}
		
		if attempt < maxActivationRetries-1 {
			time.Sleep(timeActivationRetry)
		}
	}
	
	r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to activate battery after %d attempts", maxActivationRetries))
}

// updateBatteryState updates the battery state based on current conditions
func (r *BatteryReader) updateBatteryState() {
	// For battery 1, never enable it - only poll status
	if r.index == 1 {
		return
	}

	// Call the new activateBattery method which implements the persistent retry mechanism
	r.activateBattery()
}

// sendCommand sends a command to the battery with retries
func (r *BatteryReader) sendCommand(cmd BatteryCommand) {
	// Ensure minimum time between commands
	if time.Since(r.lastCmd) < timeCmd {
		time.Sleep(timeCmd - time.Since(r.lastCmd))
	}

	data := make([]byte, 4)
	data[0] = byte(cmd)
	data[1] = byte(cmd >> 8)
	data[2] = byte(cmd >> 16)
	data[3] = byte(cmd >> 24)

	var lastErr error
	var successfulWrites int

	for retry := 0; retry < maxWriteRetries; retry++ {
		// Verify state before attempting write
		if r.hal.GetState() != hal.StatePresent {
			lastErr = fmt.Errorf("invalid state for writing: %s", r.hal.GetState())
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmdSlow) // Use slower delay for state issues
			continue
		}

		lastErr = r.hal.WriteBinary(addrCommand, data)
		if lastErr == nil {
			successfulWrites++

			// On first successful write, give a short delay before reading response
			if successfulWrites == 1 {
				time.Sleep(timeCmd)
			}

			// Read response, handling CREDIT notifications
			readData, err := r.hal.ReadBinary(addrCommand)
			if err != nil {
				lastErr = fmt.Errorf("failed to read response: %v", err)
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
				time.Sleep(timeCmd)
				continue
			}

			// Skip CREDIT notifications (0x60 0x06)
			if len(readData) >= 2 && readData[0] == 0x60 && readData[1] == 0x06 {
				// Read the actual response after the CREDIT notification
				readData, err = r.hal.ReadBinary(addrCommand)
				if err != nil {
					lastErr = fmt.Errorf("failed to read response after CREDIT: %v", err)
					r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
					time.Sleep(timeCmd)
					continue
				}
			}

			// Check for ACK response (0x0A00)
			if len(readData) >= 2 && readData[0] == 0x0A && readData[1] == 0x00 {
				r.lastCmd = time.Now()
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sent command: %v (ACK received)", cmd))
				return
			}

			// Check for RF frame corruption (all 0xFF)
			allFF := true
			for _, b := range readData {
				if b != 0xFF {
					allFF = false
					break
				}
			}
			if allFF {
				lastErr = fmt.Errorf("RF frame corruption detected")
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
				time.Sleep(timeCmdSlow) // Use slower delay for RF issues
				continue
			}

			// If we've had multiple successful writes, consider it good enough
			if successfulWrites >= 2 {
				r.lastCmd = time.Now()
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sent command: %v (accepted after %d successful writes)", cmd, successfulWrites))
				return
			}

			// No ACK and not enough successful writes, try again
			lastErr = fmt.Errorf("no ACK received")
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmd)
			continue
		}

		// Check if we lost connection
		if r.hal.GetState() != hal.StatePresent {
			lastErr = fmt.Errorf("lost connection during write: %v", lastErr)
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmdSlow) // Use slower delay for connection issues
			continue
		}

		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
		time.Sleep(timeCmd) // Standard delay for other errors
	}

	r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to send command after %d retries: %v", maxWriteRetries, lastErr))
}

// logCallback handles logging for the battery reader
func (r *BatteryReader) logCallback(level hal.LogLevel, message string) {
	r.service.logger.Printf("[Battery %d] %s", r.index, message)
}

// handleRedisSubscription handles Redis subscription for seatbox lock state
func (s *Service) handleRedisSubscription() {
	s.logger.Printf("[Redis] Subscribing to channel 'vehicle'")
	pubsub := s.redis.Subscribe(s.ctx, "vehicle")
	defer pubsub.Close()

	// Wait for confirmation that subscription is created before publishing anything
	_, err := pubsub.Receive(s.ctx)
	if err != nil {
		s.logger.Printf("[Redis] Error setting up Redis subscription: %v", err)
		return
	}
	s.logger.Printf("[Redis] Successfully subscribed to channel 'vehicle'")

	// Get initial state
	s.logger.Printf("[Redis] Fetching initial seatbox state")
	s.updateSeatboxState()

	// Listen for messages
	ch := pubsub.Channel()
	s.logger.Printf("[Redis] Starting message loop for seatbox state updates")
	for {
		select {
		case <-s.ctx.Done():
			s.logger.Printf("[Redis] Subscription loop terminated due to context cancellation")
			return
		case msg := <-ch:
			s.logger.Printf("[Redis] Received notification on channel '%s', payload: %s", msg.Channel, msg.Payload)
			if msg.Payload == "seatbox:lock" {
				s.updateSeatboxState()
			}
		}
	}
}

// updateSeatboxState gets the current seatbox state from Redis and updates battery state
func (s *Service) updateSeatboxState() {
	s.logger.Printf("[Redis] Fetching current seatbox state from Redis")
	state, err := s.redis.HGet(s.ctx, "vehicle", "seatbox:lock").Result()
	if err != nil {
		if err == redis.Nil {
			s.logger.Printf("[Redis] No seatbox state found in Redis")
		} else {
			s.logger.Printf("[Redis] Error getting seatbox state from Redis: %v", err)
		}
		return
	}

	s.logger.Printf("[Redis] Current seatbox state: %s", state)
	enabled := state == "closed"
	s.logger.Printf("[Redis] Setting battery 0 enabled state to: %v", enabled)
	s.SetEnabled(0, enabled)
}
