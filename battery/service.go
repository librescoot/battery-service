package battery

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"reflect"
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
	timeHeartbeatIntervalScooter = 1 * time.Second     // Interval for ScooterHeartbeat when present (Changed from 400ms)
	timeHeartbeatTimeoutScooter  = 800 * time.Millisecond // Timeout for battery's internal safety check
	timeReadyToScootTimeout    = 350 * time.Millisecond // Max time to wait for BatteryReadyToScoot response
	timePeriodicOnInterval     = 3 * time.Second     // Interval to send BatteryOn when seatbox closed
	timeCmd             = 200 * time.Millisecond
	timeCmdSlow         = 800 * time.Millisecond
	timeCmdFirstOpened  = 500 * time.Millisecond // Used after SeatboxClosed/Opened?
	timePresence        = 10 * time.Second
	timeCheckReader     = 10 * time.Second
	timeDeparture      = 500 * time.Millisecond
	timeBattery1Poll    = 10 * time.Minute  // Special polling interval for battery 1
	timeStateVerify     = 1000 * time.Millisecond // Time to wait before verifying state change
	timeActivationRetry = 2 * time.Second   // Time between activation retry attempts
	timeHALTimeout      = 7 * time.Second // Timeout for individual HAL operations (Increased from 5s)
	timeActiveStatusPoll = 10 * time.Second // Interval to poll status when battery is active

	// Constants for temperature limits
	temperatureStateColdLimit = -10 // Celsius
	temperatureStateHotLimit  = 60  // Celsius

	// Retry constants
	maxReadRetries     = 3 // Increased from 2
	maxWriteRetries    = 3 // Increased from 2
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
	readyToScoot bool          // Flag indicating battery responded with ReadyToScoot
	stopChan     chan struct{} // Channel to signal goroutine shutdown
	nfcMutex     sync.Mutex    // Serializes access to NFC HAL operations
	lastPublishedData BatteryData // Stores the state as of the last successful Redis update with PUBLISH
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
	debug    bool // Add debug flag here
	seatboxOpen bool // Track current seatbox state
}

// NewService creates a new battery service
func NewService(config *ServiceConfig, logger *log.Logger, debugMode bool) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())
	
	s := &Service{
		config: config,
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
		debug:  debugMode, // Store debugMode
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
	reader.hal, err = hal.NewPN7150(config.DeviceName, reader.logCallback, nil, true, false, s.debug)
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
		r.lastPublishedData = r.data // Initialize lastPublishedData to prevent false-positive changes on first update
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
			// For battery 1, only poll the tag detector less frequently when present
			r.Lock() // Lock needed to check r.data.Present safely
			shouldSkipPoll := r.index == 1 && r.data.Present && time.Since(lastBattery1Poll) < timeBattery1Poll
			r.Unlock() // Unlock after check

			if shouldSkipPoll {
				// Skip the hardware poll, but sleep briefly to check again soon
				time.Sleep(500 * time.Millisecond)
				continue
			}

			// Proceed with tag detection
			r.nfcMutex.Lock()
			tags, err := r.hal.DetectTags()
			r.nfcMutex.Unlock()
			if err != nil {
				r.logCallback(hal.LogLevelError, fmt.Sprintf("Tag detection error: %v", err))
				// Short sleep after error before retrying detection
				time.Sleep(timeDeparture) // Use a short delay like timeDeparture
				continue
			}

			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Detected %d tags", len(tags)))

			if len(tags) == 1 && (tags[0].RFProtocol == hal.RFProtocolT2T || tags[0].RFProtocol == hal.RFProtocolISODEP) {
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Found %s tag with ID: %x", tags[0].RFProtocol, tags[0].ID))
				r.handleTagPresent() // Handles state change and Redis update

				// Update last poll time for battery 1 *only after* confirming presence
				r.Lock()
				if r.index == 1 {
					lastBattery1Poll = time.Now()
				}
				r.Unlock()

			} else if len(tags) > 1 {
				r.logCallback(hal.LogLevelWarning, "Multiple tags detected, ignoring")
				r.handleTagAbsent() // Handles state change and Redis update
			} else {
				r.handleTagAbsent() // Handles state change and Redis update
			}

			// Always use a short sleep interval to ensure responsiveness for absence detection
			time.Sleep(500 * time.Millisecond)
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
		ticker := time.NewTicker(timeHeartbeatIntervalScooter)
		defer ticker.Stop()
		var lastStatusPoll time.Time // Track time of last status poll when active
		var lastBattery0Poll time.Time // Track time of last general poll for battery 0

		for {
			select {
			case <-r.stopChan:
				return
			case <-ticker.C:
				r.Lock()
				// Read state variables while holding the lock
				present := r.data.Present
				state := r.data.State
				seatboxOpen := r.service.seatboxOpen // Keep reading it for logging/potential future use
				// This check is specifically for ACTIVE batteries
				shouldPollStatus := present && state == BatteryStateActive && time.Since(lastStatusPoll) >= timeActiveStatusPoll
				r.Unlock() // Release lock before potentially blocking IO

				if present {
					if seatboxOpen {
						// --- Seatbox OPEN Logic --- 
						// Send ScooterHeartbeat periodically
						r.logCallback(hal.LogLevelDebug, "Sending ScooterHeartbeat (Seatbox Open: true)")
						if err := r.sendCommand(r.service.ctx, BatteryCommandScooterHeartbeat); err != nil {
							r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to send SCOOTER_HEARTBEAT: %v", err))
						}
					} else {
						// --- Seatbox CLOSED Logic (Implements diagram's heartbeat substate) --- 
						r.logCallback(hal.LogLevelDebug, "Heartbeat tick: Running closed seatbox maintenance cycle")
						
						// 1. Send SEATBOX_CLOSED
						if err := r.sendCommand(r.service.ctx, BatteryCommandUserClosedSeatbox); err != nil {
							r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to send SEATBOX_CLOSED in maint: %v", err))
							// Optional: goto nextIteration if this fails?
						}
						
						time.Sleep(timeCmd) // tm_closed equivalent
						
						// 2. Determine and Send ON/OFF Command
						_, expectedState, err := r.determineAndSendCommandOnOff()
						if err != nil {
							r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to send ON/OFF command in maint: %v", err))
							// Optional: goto nextIteration if this fails?
						}
						
						time.Sleep(timeCmd) // tm_on_off equivalent
						
						// 3. Read status and check state correctness
						if !r.checkStateCorrectAfterRead(expectedState) {
							// State is incorrect, attempt recovery
							r.logCallback(hal.LogLevelWarning, "State incorrect after ON/OFF, attempting recovery...")
							// 4. Send INSERTED_IN_SCOOTER (Recovery attempt)
							if err := r.sendCommand(r.service.ctx, BatteryCommandInsertedInScooter); err != nil {
								r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to send INSERTED_IN_SCOOTER for recovery: %v", err))
							}
							
							time.Sleep(timeCmd) // tm_inserted_closed equivalent
							
							// The loop will restart on the next tick, implicitly going back to send_closed
						} else {
							// State is correct, wait for next tick (wait_update equivalent)
							r.logCallback(hal.LogLevelDebug, "State correct, waiting for next maint cycle.")
						}
					}
				}

				// Separately, handle periodic status polling IF ACTIVE (for any battery)
				if shouldPollStatus {
					r.logCallback(hal.LogLevelDebug, "Polling active battery status due to interval")
					lastStatusPoll = time.Now()
					if err := r.readBatteryStatus(); err != nil {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Error during periodic status poll: %v", err))
					}
				}

				// Additionally, poll battery 0 every 10 seconds regardless of state, if present
				r.Lock() // Re-acquire lock briefly to check index and presence
				presentForB0Poll := r.data.Present
				indexIs0 := r.index == 0
				r.Unlock()

				if indexIs0 && presentForB0Poll && time.Since(lastBattery0Poll) >= timeActiveStatusPoll {
					// Avoid polling if we *just* polled because it was active
					if !shouldPollStatus || time.Since(lastStatusPoll) > 1*time.Second { // Add small buffer
						r.logCallback(hal.LogLevelDebug, "Polling battery 0 status due to 10s interval")
						lastBattery0Poll = time.Now()
						if err := r.readBatteryStatus(); err != nil {
							r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Error during battery 0 periodic poll: %v", err))
						}
					} else {
						// Active poll just ran, update general poll time to avoid immediate re-poll
						lastBattery0Poll = lastStatusPoll
					}
				}
			}
		}
	}()
}

// handleTagPresent handles a present tag
func (r *BatteryReader) handleTagPresent() {
	const maxInsertionRetries = 3
	r.Lock()

	if !r.data.Present {
		r.data.Present = true // Mark as present immediately
		r.justInserted = true
		r.readyToScoot = false // Reset flag on new insertion
		// Unlock before potentially blocking IO/polling
		r.Unlock()

		r.logCallback(hal.LogLevelInfo, "Battery inserted, attempting initialization sequence...")

		// --- Insertion Sequence --- (Retry loop)
		var insertionSuccess bool
		for attempt := 0; attempt < maxInsertionRetries; attempt++ {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Insertion attempt %d/%d", attempt+1, maxInsertionRetries))

			// 1. Send BatteryInsertedInScooter command
			if err := r.sendCommand(r.service.ctx, BatteryCommandInsertedInScooter); err != nil {
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to send INSERTED command: %v. Retrying...", err))
				time.Sleep(timeCmdSlow) // Wait before retrying command
				continue
			}

			// Add delay before polling for response
			time.Sleep(timeCmd)

			// 2. Wait for BatteryReadyToScoot response
			ready, err := r.readCommandRegisterWithTimeout(r.service.ctx, timeReadyToScootTimeout, BatteryCommandReadyToScoot)
			if err != nil {
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Error waiting for READY_TO_SCOOT: %v. Retrying sequence...", err))
				// No need to sleep here, sendCommand already enforces delay, and timeout adds delay
				continue
			}

			if ready {
				r.logCallback(hal.LogLevelInfo, "Battery reported READY_TO_SCOOT.")
				insertionSuccess = true
				break // Success!
			} else {
				// This case should theoretically be covered by the timeout error in readCommandRegisterWithTimeout
				r.logCallback(hal.LogLevelWarning, "Did not receive READY_TO_SCOOT within timeout. Retrying sequence...")
				// Continue to next attempt
			}
		}

		// --- Post-Insertion --- 
		if !insertionSuccess {
			r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed insertion sequence after %d attempts.", maxInsertionRetries))
			// Re-acquire lock briefly to update fault/state
			r.Lock()
			r.data.Present = false // Mark as not present if sequence failed
			r.data.Faults.CommunicationError = true
			_ = r.updateRedisStatus() // Update redis about the failure
			r.Unlock()
			return
		}

		// Insertion successful, now read initial status
		if err := r.readBatteryStatus(); err != nil {
			r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to read battery status after insertion: %v", err))
			// Re-acquire lock briefly to update fault
			r.Lock()
			r.data.EmptyOr0Data++ // Or maybe CommunicationError?
			_ = r.updateRedisStatus()
			r.Unlock()
			// Proceed even if status read fails initially? Spec implies battery is Idle.
		}

		// Re-acquire lock to update state and log
		r.Lock()
		r.justInserted = false
		r.readyToScoot = true
		r.data.State = BatteryStateIdle // Per spec step 6
		r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery initialized: serial_number=%s, fw_version=%s, charge=%d, state=%s",
			string(r.data.SerialNumber[:]), r.data.FWVersion, r.data.Charge, r.data.State))
		// Update Redis with initial state (Idle)
		if err := r.updateRedisStatus(); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after insertion: %v", err))
		}
		
		// Get current seatbox state AFTER updating reader state
		seatboxIsOpen := r.service.seatboxOpen
		shouldActivateNow := r.index == 0 && !seatboxIsOpen && r.readyToScoot
		
		r.Unlock() // Unlock before potentially calling activateBattery

		// If seatbox is already closed and this is battery 0, activate now
		if shouldActivateNow {
			r.logCallback(hal.LogLevelInfo, "Battery 0 ready and seatbox closed. Activating immediately.")
			r.activateBattery()
		}

		return // Initial insertion handling complete for this cycle
	}

	// --- Handle case where tag was already present --- 

	// Calculate temperature state (safe to do under lock)
	oldTempState := r.data.TemperatureState
	r.data.TemperatureState = r.calculateTemperatureState()
	if oldTempState != r.data.TemperatureState {
		r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery temperature-state: %s", r.data.TemperatureState))
		// Update redis if temp state changed?
		if err := r.updateRedisStatus(); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis for temp state: %v", err))
		}
	}

	r.Unlock() // Unlock as we are done with reader data for now
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

// readCommandRegisterWithTimeout attempts to read the command register within a timeout,
// specifically looking for a target command response written by the battery.
func (r *BatteryReader) readCommandRegisterWithTimeout(ctx context.Context, timeout time.Duration, targetCmd BatteryCommand) (bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(50 * time.Millisecond) // Poll frequency
	defer ticker.Stop()

	for {
		select {
		case <-readCtx.Done():
			return false, fmt.Errorf("timeout waiting for command 0x%X: %w", targetCmd, readCtx.Err())
		case <-ticker.C:
			// Use readRegisterWithRetry but specifically for addrCommand
			// We need the raw data, not just verification success
			// Let's simplify for now: try a direct read within the overall timeout
			// halCtx, halCancel := context.WithTimeout(readCtx, timeHALTimeout) // Unused, ReadBinary doesn't take context
			r.nfcMutex.Lock()
			data, err := r.hal.ReadBinary(addrCommand) // ReadBinary uses its own internal timeouts/handling
			r.nfcMutex.Unlock()
			// halCancel() // Removed corresponding cancel

			if err != nil {
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Error reading command register: %v", err))
				// Don't return error immediately, let the timeout handle persistent failures
				continue
			}

			// Check if response matches target command (expecting 4 bytes)
			if len(data) >= 4 {
				readCmd := BatteryCommand(uint32(data[3])<<24 | uint32(data[2])<<16 | uint32(data[1])<<8 | uint32(data[0]))
				if readCmd == targetCmd {
					r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Received expected command response: 0x%X", targetCmd))
					return true, nil
				} else {
					r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Read command register: 0x%X (expected 0x%X)", readCmd, targetCmd))
				}
			} else {
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Read command register (short): %X", data))
			}
		}
	}
}

// readRegisterWithRetry reads a register with retries and verification
func (r *BatteryReader) readRegisterWithRetry(ctx context.Context, addr uint16) ([]byte, error) {
	var lastErr error
	var data, checkData []byte
	var verified bool
	var successfulReads int

	// Reset communication error flag at the start of the attempt sequence
	r.data.Faults.CommunicationError = false

	for retry := 0; retry < maxReadRetries; retry++ {
		// Check overall context cancellation
		select {
		case <-ctx.Done():
			lastErr = fmt.Errorf("context cancelled before read retry %d: %w", retry+1, ctx.Err())
			goto endReadRetryLoop
		default:
		}

		// First read attempt
		r.nfcMutex.Lock()
		data, lastErr = r.hal.ReadBinary(addr)
		r.nfcMutex.Unlock()

		if lastErr != nil {
			// Check if context timed out externally (if HAL call didn't respect it internally)
			select {
			case <-ctx.Done():
				if ctx.Err() == context.DeadlineExceeded {
					lastErr = fmt.Errorf("context deadline exceeded during read on retry %d: %w", retry+1, ctx.Err())
				} else {
					lastErr = fmt.Errorf("context error during read on retry %d: %w", retry+1, ctx.Err())
				}
			default:
			}

			// Check if we lost connection
			if r.hal.GetState() != hal.StatePresent {
				lastErr = fmt.Errorf("lost connection during read: %v", lastErr)
				// Add a longer delay and attempt to recover the connection
				time.Sleep(timeCmdFirstOpened)
				if r.hal.GetState() == hal.StateDiscovering {
					// Wait for potential rediscovery
					time.Sleep(timeCmdSlow) // Replaced timeCmdFirstAsleep
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
		halCtxVerify, cancelVerify := context.WithTimeout(ctx, timeHALTimeout)
		r.nfcMutex.Lock()
		checkData, lastErr = r.hal.ReadBinary(addr)
		r.nfcMutex.Unlock()
		cancelVerify()

		if lastErr != nil {
			// Check if context timed out externally
			select {
			case <-halCtxVerify.Done():
				if halCtxVerify.Err() == context.DeadlineExceeded {
					lastErr = fmt.Errorf("hal.ReadBinary verification timeout on retry %d: %w", retry+1, halCtxVerify.Err())
				}
			default:
			}

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

endReadRetryLoop: // Label to jump to for final error handling

	if !verified {
		err := fmt.Errorf("failed to read register 0x%04x after %d retries: %w", addr, maxReadRetries, lastErr)
		r.logCallback(hal.LogLevelError, err.Error()) // Log the final error
		// Set communication error fault if verification failed after retries
		r.data.Faults.CommunicationError = true
		if updateErr := r.updateRedisStatus(); updateErr != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after read communication error: %v", updateErr))
		}
		return nil, err
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

	// Clear communication error flag on success
	if r.data.Faults.CommunicationError {
		r.data.Faults.CommunicationError = false
		if updateErr := r.updateRedisStatus(); updateErr != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after clearing read communication error: %v", updateErr))
		}
	}

	return data, nil
}

// faultCodeMap maps boolean fault fields in BatteryFaults to their numeric codes
var faultCodeMap = map[string]int{
	"ChargeTempOverHigh":    0,
	"ChargeTempOverLow":     1,
	"DischargeTempOverHigh": 2,
	"DischargeTempOverLow":  3,
	"SignalWireBroken":      4,
	"SecondLevelOverTemp":   5,
	"PackVoltageHigh":       6,
	"MOSTempOverHigh":       7,
	"CellVoltageHigh":       8,
	"PackVoltageLow":        9,
	"CellVoltageLow":        10,
	"ChargeOverCurrent":     11,
	"DischargeOverCurrent":  12,
	"ShortCircuit":          13,
}

// updateRedisStatus updates the Redis hash and fault set with current battery status
func (r *BatteryReader) updateRedisStatus() error {
	if r.service.redis == nil {
		return fmt.Errorf("Redis client not initialized")
	}

	key := fmt.Sprintf("battery:%d", r.index)
	faultSetKey := fmt.Sprintf("battery:%d:fault", r.index)
	faultNotifyChannel := key + " fault" // Publish to 'battery:X fault' channel

	// Create a map for the main battery status fields
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

	// Fetch current fault set members from Redis
	currentFaultCodesStr, err := r.service.redis.SMembers(r.service.ctx, faultSetKey).Result()
	if err != nil && err != redis.Nil {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to get current faults from Redis set %s: %v", faultSetKey, err))
		currentFaultCodesStr = []string{} // Treat as empty if error or nil
	}
	currentFaultsInSet := make(map[string]struct{})
	for _, codeStr := range currentFaultCodesStr {
		currentFaultsInSet[codeStr] = struct{}{}
	}

	// --- Use a pipeline for atomic execution (MULTI/EXEC) ---
	pipe := r.service.redis.Pipeline()
	faultsChanged := false

	// --- Publish for specific field changes on 'battery:X' channel ---
	// Compare current r.data with r.lastPublishedData

	// Present
	if r.data.Present != r.lastPublishedData.Present {
		pipe.Publish(r.service.ctx, key, "present")
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Publishing 'present' to %s (new: %v, old: %v)", key, r.data.Present, r.lastPublishedData.Present))
	}
	// State
	if r.data.State != r.lastPublishedData.State {
		pipe.Publish(r.service.ctx, key, "state")
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Publishing 'state' to %s (new: %s, old: %s)", key, r.data.State, r.lastPublishedData.State))
	}
	// Charge
	if r.data.Charge != r.lastPublishedData.Charge {
		pipe.Publish(r.service.ctx, key, "charge")
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Publishing 'charge' to %s (new: %d, old: %d)", key, r.data.Charge, r.lastPublishedData.Charge))
	}
	// TemperatureState
	if r.data.TemperatureState != r.lastPublishedData.TemperatureState {
		pipe.Publish(r.service.ctx, key, "temperature-state")
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Publishing 'temperature-state' to %s (new: %s, old: %s)", key, r.data.TemperatureState, r.lastPublishedData.TemperatureState))
	}

	// 1. Set all main status fields in the hash
	pipe.HMSet(r.service.ctx, key, status)

	// 2. Handle BMS fault codes using the Set
	// Use reflection to iterate over the BatteryFaults struct fields mapped in faultCodeMap
	faultsValue := reflect.ValueOf(r.data.Faults)
	for fieldName, faultCode := range faultCodeMap {
		fieldValue := faultsValue.FieldByName(fieldName)
		if !fieldValue.IsValid() || fieldValue.Kind() != reflect.Bool {
			continue // Should not happen if map is correct
		}
		
		isFaultActive := fieldValue.Bool()
		faultCodeStr := fmt.Sprintf("%d", faultCode)
		_, faultWasInSet := currentFaultsInSet[faultCodeStr]

		if isFaultActive && !faultWasInSet {
			// Fault is active now, but wasn't in the set -> SADD
			pipe.SAdd(r.service.ctx, faultSetKey, faultCodeStr)
			faultsChanged = true
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Adding fault code %d (%s) to set %s", faultCode, fieldName, faultSetKey))
		} else if !isFaultActive && faultWasInSet {
			// Fault is inactive now, but was in the set -> SREM
			pipe.SRem(r.service.ctx, faultSetKey, faultCodeStr)
			faultsChanged = true
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Removing fault code %d (%s) from set %s", faultCode, fieldName, faultSetKey))
		}
	}
	
	// 3. Publish notification *if* faults changed
	if faultsChanged {
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Fault set %s changed, publishing notification to %s", faultSetKey, faultNotifyChannel))
		pipe.Publish(r.service.ctx, faultNotifyChannel, "fault")
	}

	// Execute the pipeline
	_, execErr := pipe.Exec(r.service.ctx)
	if execErr != nil {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Redis pipeline execution failed: %v", execErr))
		return fmt.Errorf("redis pipeline execution failed: %v", execErr) // Return error to indicate failure, DO NOT update lastPublishedData
	}

	// --- After successful pipeline execution, update lastPublishedData ---
	r.lastPublishedData = r.data // Update to the new successfully written state

	r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Updated Redis status for %s and fault set %s. Published relevant changes.", key, faultSetKey))
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

		data, err = r.readRegisterWithRetry(r.service.ctx, addrStatus0)
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

		data, err = r.readRegisterWithRetry(r.service.ctx, addrStatus1)
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

		data, err = r.readRegisterWithRetry(r.service.ctx, addrStatus2)
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

// activateBattery now simply sends the ON command. It's called when appropriate
// (e.g., for battery 0 when seatbox closes and battery is ready).
func (r *BatteryReader) activateBattery() {
	// For battery 1, never send ON command
	if r.index == 1 {
		r.logCallback(hal.LogLevelDebug, "activateBattery called for battery 1 - skipping ON command.")
		return
	}

	r.Lock()
	state := r.data.State
	ready := r.readyToScoot
	lowSOC := r.data.LowSOC
	enabled := r.enabled // Keep enabled check? Spec doesn't mention it explicitly here.
	r.Unlock()

	if !enabled {
		r.logCallback(hal.LogLevelDebug, "activateBattery: reader disabled, skipping ON command.")
		// Should we send OFF here if it's not Idle/Asleep? Let's keep it simple for now.
		return
	}

	if lowSOC {
		r.logCallback(hal.LogLevelDebug, "activateBattery: Low SOC, skipping ON command.")
		// Send OFF if not Idle/Asleep?
		if state != BatteryStateAsleep && state != BatteryStateIdle {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sending OFF command (low SOC activation blocked): current_state=%s", state))
			_ = r.sendCommand(r.service.ctx, BatteryCommandOff)
		}
		return
	}

	if !ready {
		r.logCallback(hal.LogLevelDebug, "activateBattery: Not ReadyToScoot, skipping ON command.")
		return
	}

	// If already active, nothing to do
	if state == BatteryStateActive {
		r.logCallback(hal.LogLevelDebug, "activateBattery: Already active.")
		return
	}

	r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Sending ON command (current state: %s)", state))
	if err := r.sendCommand(r.service.ctx, BatteryCommandOn); err != nil {
		r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to send ON command: %v", err))
		// TODO: Should we retry or set a fault here? Let's log for now.
		return
	}

	// Wait briefly and re-read status to verify activation (optional but good practice)
	time.Sleep(timeStateVerify)
	if err := r.readBatteryStatus(); err != nil {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to verify state after ON command: %v", err))
	} else {
		r.Lock()
		newState := r.data.State
		r.Unlock()
		if newState == BatteryStateActive {
			r.logCallback(hal.LogLevelInfo, "Battery successfully activated (state verified).")
		} else {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery state is %s after ON command (expected Active).", newState))
			// Set fault?
			r.Lock()
			r.data.Faults.NotFollowingCommand = true
			_ = r.updateRedisStatus()
			r.Unlock()
		}
	}
}

// Helper function for determining ON/OFF command based on current state and conditions
func (r *BatteryReader) determineAndSendCommandOnOff() (BatteryCommand, BatteryState, error) {
	r.Lock()
	currentState := r.data.State
	ready := r.readyToScoot
	lowSOC := r.data.LowSOC
	enabled := r.enabled
	index := r.index
	r.Unlock()

	// Default expectations
	sentCmd := BatteryCommandNone
	expectedState := currentState // Expect state to remain unchanged if no command sent
	err := fmt.Errorf("no command needed")

	// --- Determine desired state and command --- 

	// Conditions where OFF is desired
	if !enabled || lowSOC {
		if currentState != BatteryStateAsleep && currentState != BatteryStateIdle {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for OFF met (enabled=%v, lowSOC=%v), current state=%s. Sending OFF.", enabled, lowSOC, currentState))
			sentCmd = BatteryCommandOff
			expectedState = BatteryStateIdle // Or Asleep? Idle seems more likely after OFF cmd
			err = r.sendCommand(r.service.ctx, sentCmd)
		} else {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for OFF met but already Asleep/Idle (enabled=%v, lowSOC=%v). Expecting %s.", enabled, lowSOC, currentState))
			expectedState = currentState // Already in a low power state
		}
		return sentCmd, expectedState, err
	}

	// Conditions where ON is desired (only for battery 0)
	if index == 0 && ready {
		if currentState != BatteryStateActive {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for ON met (index=%d, ready=%v), current state=%s. Sending ON.", index, ready, currentState))
			sentCmd = BatteryCommandOn
			expectedState = BatteryStateActive
			err = r.sendCommand(r.service.ctx, sentCmd)
		} else {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for ON met but already Active (index=%d, ready=%v). Expecting Active.", index, ready))
			expectedState = BatteryStateActive // Already active
		}
		return sentCmd, expectedState, err
	}

	// Default case: No command needed (e.g., battery 1 is present and ready, or battery 0 not ready)
	r.logCallback(hal.LogLevelDebug, fmt.Sprintf("No ON/OFF command needed (index=%d, ready=%v, state=%s). Expecting %s.", index, ready, currentState, expectedState))
	return sentCmd, expectedState, err
}

// Helper function to read status and check if current state matches expected state
func (r *BatteryReader) checkStateCorrectAfterRead(expectedState BatteryState) bool {
	if err := r.readBatteryStatus(); err != nil {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to read status for state check: %v", err))
		return false // Cannot verify state if read fails
	}
	r.Lock()
	currentState := r.data.State
	r.Unlock()

	match := currentState == expectedState

	// Handle case where OFF command leads to Asleep instead of Idle
	if !match && (expectedState == BatteryStateIdle) && (currentState == BatteryStateAsleep) {
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("State check: expected %s, got %s (Accepting Asleep as valid outcome for Idle expectation)", expectedState, currentState))
		match = true
	}

	if !match {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("State mismatch after read: expected %s, got %s", expectedState, currentState))
	} else {
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("State match after read: expected %s, got %s", expectedState, currentState))
	}
	return match
}

// sendCommand sends a command to the battery with retries
func (r *BatteryReader) sendCommand(ctx context.Context, cmd BatteryCommand) error {
	// Ensure minimum time between commands
	if time.Since(r.lastCmd) < timeCmd {
		sleepCtx, cancelSleep := context.WithTimeout(ctx, timeCmd - time.Since(r.lastCmd))
		select {
		case <-sleepCtx.Done():
			cancelSleep()
			if sleepCtx.Err() != context.DeadlineExceeded {
				return fmt.Errorf("context cancelled during command delay: %w", sleepCtx.Err())
			}
			// If timeout expired, just continue, minimum time passed
		case <-time.After(timeCmd - time.Since(r.lastCmd)):
			// Normal sleep completion
		}
		cancelSleep()
	}

	data := make([]byte, 4)
	data[0] = byte(cmd)
	data[1] = byte(cmd >> 8)
	data[2] = byte(cmd >> 16)
	data[3] = byte(cmd >> 24)

	var lastErr error
	var successfulWrites int

	// Reset communication error flag at the start of the attempt sequence
	r.data.Faults.CommunicationError = false

	for retry := 0; retry < maxWriteRetries; retry++ {
		// Check overall context cancellation
		select {
		case <-ctx.Done():
			lastErr = fmt.Errorf("context cancelled before write retry %d: %w", retry+1, ctx.Err())
			goto endWriteRetryLoop
		default:
		}

		// Verify state before attempting write
		if r.hal.GetState() != hal.StatePresent {
			lastErr = fmt.Errorf("invalid state for writing: %s", r.hal.GetState())
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmdSlow) // Use slower delay for state issues
			continue
		}

		// Create context with timeout for HAL write call
		halWriteCtx, cancelWrite := context.WithTimeout(ctx, timeHALTimeout)
		r.nfcMutex.Lock()
		lastErr = r.hal.WriteBinary(addrCommand, data) // Pass halWriteCtx if HAL supports it
		r.nfcMutex.Unlock()
		cancelWrite()

		if lastErr == nil {
			successfulWrites++

			// On first successful write, give a short delay before reading response
			if successfulWrites == 1 {
				sleepCtx, cancelSleep := context.WithTimeout(ctx, timeCmd)
				select {
				case <-sleepCtx.Done():
					cancelSleep()
					lastErr = fmt.Errorf("context cancelled during write-read delay: %w", sleepCtx.Err())
					continue // Go to next retry
				case <-time.After(timeCmd):
					// Normal sleep completion
				}
				cancelSleep()
			}

			// Create context with timeout for HAL read call
			halReadCtx, cancelRead := context.WithTimeout(ctx, timeHALTimeout)
			r.nfcMutex.Lock()
			readData, err := r.hal.ReadBinary(addrCommand) // Pass halReadCtx if HAL supports it
			r.nfcMutex.Unlock()
			cancelRead()

			if err != nil {
				// Check if context timed out externally
				select {
				case <-halReadCtx.Done():
					if halReadCtx.Err() == context.DeadlineExceeded {
						lastErr = fmt.Errorf("hal.ReadBinary response timeout on retry %d: %w", retry+1, halReadCtx.Err())
					} else {
						lastErr = fmt.Errorf("context error during read response on retry %d: %w", retry+1, halReadCtx.Err())
					}
				default:
					lastErr = fmt.Errorf("failed to read response: %w", err)
				}
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
				time.Sleep(timeCmd)
				continue
			}

			// Skip CREDIT notifications (0x60 0x06)
			if len(readData) >= 2 && readData[0] == 0x60 && readData[1] == 0x06 {
				// Read the actual response after the CREDIT notification
				halReadCreditCtx, cancelReadCredit := context.WithTimeout(ctx, timeHALTimeout)
				r.nfcMutex.Lock()
				readData, err = r.hal.ReadBinary(addrCommand) // Pass halReadCreditCtx if HAL supports it
				r.nfcMutex.Unlock()
				cancelReadCredit()

				if err != nil {
					// Check if context timed out externally
					select {
					case <-halReadCreditCtx.Done():
						if halReadCreditCtx.Err() == context.DeadlineExceeded {
							lastErr = fmt.Errorf("hal.ReadBinary after CREDIT timeout on retry %d: %w", retry+1, halReadCreditCtx.Err())
						} else {
							lastErr = fmt.Errorf("context error during read after CREDIT on retry %d: %w", retry+1, halReadCreditCtx.Err())
						}
					default:
						lastErr = fmt.Errorf("failed to read response after CREDIT: %w", err)
					}
					r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
					time.Sleep(timeCmd)
					continue
				}
			}

			// Check for ACK response (0x0A00)
			if len(readData) >= 2 && readData[0] == 0x0A && readData[1] == 0x00 {
				r.lastCmd = time.Now()
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sent command: %v (ACK received)", cmd))
				// Clear communication error on success
				if r.data.Faults.CommunicationError {
					r.data.Faults.CommunicationError = false
					if updateErr := r.updateRedisStatus(); updateErr != nil {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after clearing write communication error: %v", updateErr))
					}
				}
				return nil // Command succeeded
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
				// Clear communication error on success (even if no ACK, write went through)
				if r.data.Faults.CommunicationError {
					r.data.Faults.CommunicationError = false
					if updateErr := r.updateRedisStatus(); updateErr != nil {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after clearing write communication error: %v", updateErr))
					}
				}
				return nil // Command succeeded (conditionally)
			}

			// No ACK and not enough successful writes, try again
			lastErr = fmt.Errorf("no ACK received")
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: %v", retry+1, lastErr))
			time.Sleep(timeCmd)
		} else { // lastErr != nil
			// Check if the timeout was exceeded before the error occurred
			if halWriteCtx.Err() == context.DeadlineExceeded { // Check error directly
				 lastErr = fmt.Errorf("hal.WriteBinary timeout or context exceeded before error on retry %d: %w", retry+1, halWriteCtx.Err())
			} // else keep original lastErr or handle other ctx errors if needed

			// Log error with potentially updated lastErr
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Retry %d: WriteBinary failed: %v", retry+1, lastErr))

			// Check if we lost connection
			if r.hal.GetState() != hal.StatePresent {
				lastErr = fmt.Errorf("lost connection during write: %v", lastErr) // Keep updated error if timeout occurred

				// Add a longer delay and attempt to recover the connection
				time.Sleep(timeCmdFirstOpened)
				if r.hal.GetState() == hal.StateDiscovering {
					// Wait for potential rediscovery
					time.Sleep(timeCmdSlow) // Replaced timeCmdFirstAsleep
				}
				continue
			}
		}
	}

endWriteRetryLoop: // Label to jump to for final error handling

	finalErr := fmt.Errorf("failed to send command %v after %d retries: %w", cmd, maxWriteRetries, lastErr)
	r.logCallback(hal.LogLevelError, finalErr.Error())
	// Set communication error fault if command failed after retries
	r.data.Faults.CommunicationError = true
	if updateErr := r.updateRedisStatus(); updateErr != nil {
		r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after write communication error: %v", updateErr))
	}
	return finalErr // Return the final error
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

	// Force the handler to run for the initial state by temporarily setting
	// the previous state to the opposite, ensuring the change is detected.
	s.Lock()
	initialState := s.seatboxOpen
	s.seatboxOpen = !initialState // Pretend the state was the opposite before the first real check
	s.Unlock()
	s.handleSeatboxStateChange(initialState) // Now run the handler with the actual initial state

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
			// Re-check seatbox state on any notification on the vehicle channel
			// as other keys might imply a seatbox change.
			// if msg.Payload == "seatbox:lock" {
			s.updateSeatboxState()
			// }
		}
	}
}

// updateSeatboxState gets the current seatbox state from Redis and updates battery state
func (s *Service) updateSeatboxState() {
	s.logger.Printf("[Redis] Fetching current seatbox state from Redis")
	state, err := s.redis.HGet(s.ctx, "vehicle", "seatbox:lock").Result()
	if err != nil {
		if err == redis.Nil {
			s.logger.Printf("[Redis] No seatbox state found in Redis, assuming closed.")
			state = "closed" // Default to closed if not found
		} else {
			s.logger.Printf("[Redis] Error getting seatbox state from Redis: %v", err)
			return
		}
	}

	s.logger.Printf("[Redis] Current seatbox state: %s", state)
	isOpen := state != "closed"

	// Call the central handler
	s.handleSeatboxStateChange(isOpen)
}

// --- Seatbox State Change Handling ---

func (s *Service) handleSeatboxStateChange(isOpen bool) {
	s.Lock()
	previousStateOpen := s.seatboxOpen
	s.seatboxOpen = isOpen
	// Collect readers to notify outside the service lock
	readersToNotify := make([]*BatteryReader, 0, 2)
	for _, r := range s.readers {
		if r != nil {
			readersToNotify = append(readersToNotify, r)
		}
	}
	s.Unlock()

	if isOpen == previousStateOpen {
		s.logger.Printf("[Seatbox] State hasn't changed (%v), ignoring.", isOpen)
		return // No change
	}

	s.logger.Printf("[Seatbox] State changed: open=%v", isOpen)

	for _, r := range readersToNotify {
		r.handleSeatboxState(isOpen)
	}
}

// handleSeatboxState is called on the BatteryReader when the seatbox state changes
func (r *BatteryReader) handleSeatboxState(isOpen bool) {
	r.Lock()
	present := r.data.Present
	ready := r.readyToScoot
	state := r.data.State
	r.Unlock()

	if !present {
		return // Ignore if battery not present
	}

	if isOpen {
		r.logCallback(hal.LogLevelInfo, "Seatbox opened")
		// Send UserOpenedSeatbox command
		if err := r.sendCommand(r.service.ctx, BatteryCommandUserOpenedSeatbox); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to send SEATBOX_OPENED: %v", err))
		}
		// Heartbeats will start automatically in the startHeartbeat loop based on service.seatboxOpen
		// Battery's internal safety check should start upon receiving heartbeat (battery-side logic)
	} else {
		r.logCallback(hal.LogLevelInfo, "Seatbox closed")
		// Send UserClosedSeatbox command
		if err := r.sendCommand(r.service.ctx, BatteryCommandUserClosedSeatbox); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to send SEATBOX_CLOSED: %v", err))
		}
		// Heartbeats will stop automatically in the startHeartbeat loop
		// Battery's internal safety check should stop (battery-side logic)

		// For Battery 0 only: If ready and not already active, send ON command
		if r.index == 0 && ready && state != BatteryStateActive {
			r.logCallback(hal.LogLevelInfo, "Seatbox closed, battery 0 ready. Sending ON command.")
			// Call activateBattery which now just sends ON
			r.activateBattery()
		}
	}
}
