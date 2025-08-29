package battery

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"battery-service/nfc/hal"
)

// Start starts the battery reader
func (r *BatteryReader) Start() error {
	// Enable the reader by default
	r.enabled = true
	r.isPoweredDown = false // Ensure we start with the reader powered up (0 = not powered down)
	
	// Initialize operation context for cancelling operations when tag departs
	r.swapOperationContext()

	// Initialize NFC HAL
	if err := r.hal.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize NFC HAL: %v", err)
	}

	// Start the state machine
	r.stateMachine.Start()

	// Start tag monitoring
	go r.monitorTags()

	// Start heartbeat
	r.startHeartbeat()

	return nil
}

// Stop stops the battery reader
func (r *BatteryReader) Stop() {
	// Stop the state machine first
	r.stateMachine.Stop()

	// Cancel any pending operations
	r.contextMutex.Lock()
	if r.operationCancel != nil {
		r.operationCancel()
	}
	r.contextMutex.Unlock()

	close(r.stopChan)

	// Reset powered down state to ensure final deinitialization works
	r.dataMutex.Lock()
	wasPoweredDown := r.isPoweredDown
	r.isPoweredDown = false
	r.dataMutex.Unlock()

	// If it was powered down, we need to initialize it first before deinitializing
	if wasPoweredDown {
		if err := r.hal.Initialize(); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to initialize HAL during shutdown: %v", err))
		}
	}

	r.hal.Deinitialize()
}

// setEnabled enables or disables the battery reader
func (r *BatteryReader) setEnabled(enabled bool) {
	r.dataMutex.Lock()
	r.enabled = enabled
	r.dataMutex.Unlock()

	// Send appropriate event to state machine
	if enabled {
		r.stateMachine.SendEvent(EventEnabled)
	} else {
		r.stateMachine.SendEvent(EventDisabled)
	}
}

// monitorTags continuously monitors for tag presence using channel-based detection
func (r *BatteryReader) monitorTags() {
	// Initialize battery state as not present
	r.dataMutex.Lock()
	if !r.data.Present {
		r.data = BatteryData{}       // Ensure clean state
		r.lastPublishedData = r.data // Initialize lastPublishedData to prevent false-positive changes on first update
		if err := r.updateRedisStatus(); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to initialize Redis status: %v", err))
		}
	}
	r.dataMutex.Unlock()

	for {
		// Get the tag event channel from HAL
		tagEventChan := r.hal.GetTagEventChannel()
		
		select {
		case <-r.stopChan:
			return
			
		case <-time.After(1 * time.Second):
			// Timeout to refresh channel periodically (handles channel recreation after HAL reinit)
			continue

		case event, ok := <-tagEventChan:
			if !ok {
				// Channel closed, HAL might be reinitializing
				r.logCallback(hal.LogLevelWarning, "Tag event channel closed, waiting for HAL reinit")
				time.Sleep(100 * time.Millisecond)
				continue
			}
			
			// Check if the reader is temporarily powered down
			r.dataMutex.RLock()
			isPoweredDown := r.isPoweredDown
			r.dataMutex.RUnlock()

			if isPoweredDown {
				// Ignore events when powered down
				continue
			}

			// Check if the event contains a serious error
			if event.Error != nil && (strings.Contains(event.Error.Error(), "unexpected reset") || 
				strings.Contains(event.Error.Error(), "serious error")) {
				r.logCallback(hal.LogLevelError, fmt.Sprintf("HAL reported serious error: %v", event.Error))
				// Trigger HAL error event in state machine
				r.stateMachine.SendEvent(EventHALError)
				continue
			}

			switch event.Type {
			case hal.TagArrival:
				if event.Tag != nil {
					r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Tag arrived: %s tag with ID: %x", event.Tag.RFProtocol, event.Tag.ID))
					
					// Check if it's a valid battery tag
					if event.Tag.RFProtocol == hal.RFProtocolT2T || event.Tag.RFProtocol == hal.RFProtocolISODEP {
						// Send tag arrived event for discovery states
						r.stateMachine.SendEvent(EventTagArrived)
						r.handleTagPresent() // Handles state change and Redis update
					} else {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Unsupported tag protocol: %s", event.Tag.RFProtocol))
						r.handleTagAbsent() // Treat unsupported tags as absent
					}
				} else {
					r.logCallback(hal.LogLevelWarning, "Tag arrival event received but tag data is nil")
				}

			case hal.TagDeparture:
				// Cancel any ongoing operations immediately and create new context
				r.swapOperationContext()
				r.handleTagAbsent() // Handles state change and Redis update
			}

		}
	}
}

// handleTagPresent handles a present tag
func (r *BatteryReader) handleTagPresent() {
	r.dataMutex.Lock()

	if !r.data.Present {
		r.data.Present = true // Mark as present immediately
		r.halReinitCount = 0 // Reset HAL reinit counter for fresh battery
		r.dataMutex.Unlock()

		r.logCallback(hal.LogLevelInfo, "Battery inserted")
		// No need to send event here - monitorTags already sent EventTagArrived
		return
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

	r.dataMutex.Unlock() // Unlock as we are done with reader data for now
}

// handleTagAbsent handles an absent tag
func (r *BatteryReader) handleTagAbsent() {
	r.dataMutex.Lock()
	defer r.dataMutex.Unlock()

	// Don't process tag absence if HAL is intentionally powered down
	if r.isPoweredDown {
		r.logCallback(hal.LogLevelDebug, "Ignoring tag absence - HAL is powered down")
		return
	}

	// Only send the departure event if the battery was previously considered present.
	// This prevents spamming the event queue.
	if r.data.Present {
		r.logCallback(hal.LogLevelInfo, "Tag departure detected, signaling state machine.")
		// The ONLY responsibility of this function is to send the event.
		// The state machine will handle the logic of confirming the departure and updating state.
		r.stateMachine.SendEvent(EventTagDeparted)
	}
}

// readBatteryStatus reads the battery status registers
func (r *BatteryReader) readBatteryStatus() error {
	const maxAttemptsAfterHALRecreation = 2 // Initial attempt + 1 retry after HAL recreation
	var lastMainError error

	for attempt := 1; attempt <= maxAttemptsAfterHALRecreation; attempt++ {
		if attempt > 1 {
			r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Retrying readBatteryStatus (attempt %d/%d) after HAL issue.", attempt, maxAttemptsAfterHALRecreation))
		}

		// Ensure we're in the correct state for reading at the start of each attempt
		// Lock is needed here to safely access r.hal
		r.nfcMutex.Lock()
		halState := r.hal.GetState()
		r.nfcMutex.Unlock()

		if halState != hal.StatePresent {
			// If the HAL is in discovery, it's a definitive sign the tag is gone.
			// Send the departure event and return an error that indicates this.
			if halState == hal.StateDiscovering {
				r.logCallback(hal.LogLevelInfo, "HAL is in Discovering state; treating as tag departure.")
				r.stateMachine.SendEvent(EventTagDeparted)
				return fmt.Errorf("tag departed, HAL is in discovering state")
			}
			
			lastMainError = fmt.Errorf("attempt %d: invalid HAL state for reading: %s", attempt, halState)
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("%s. HAL state: %s", lastMainError.Error(), halState))
			if attempt < maxAttemptsAfterHALRecreation {
				r.logCallback(hal.LogLevelWarning, "Attempting HAL recovery.")

				if err := r.simpleHALRecovery(); err != nil {
					return fmt.Errorf("%w; and HAL recovery failed: %v", lastMainError, err)
				}

				continue // Retry readBatteryStatus full sequence
			}
			return lastMainError // Max attempts reached or other unrecoverable state issue
		}

		var data []byte
		var err error

		// Read Status0
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Attempt %d: Reading Status0...", attempt))
		
		// Use operation context so reads can be cancelled when tag departs
		data, err = r.readRegisterWithRetry(r.getOperationContext(), addrStatus0)
		if err != nil {
			lastMainError = fmt.Errorf("attempt %d: failed to read Status0: %w", attempt, err)
			
			// Don't retry HAL recovery for context cancelled errors (tag departed)
			if strings.Contains(err.Error(), "context cancel") {
				r.logCallback(hal.LogLevelDebug, "Status0 read cancelled due to tag departure, not retrying")
				return lastMainError
			}
			
			if errors.Is(err, errHALRecreatedRetryRead) && attempt < maxAttemptsAfterHALRecreation {
				r.logCallback(hal.LogLevelWarning, "HAL recreated during Status0 read, retrying entire status read.")
				continue // Retry the entire readBatteryStatus sequence
			}
			return lastMainError // Permanent error or max attempts for HAL recreation reached
		}

		// Process Status0 data
		r.data.Present = true // If Status0 read is successful, tag is present
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

		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Status0 decoded: voltage=%dmV current=%dmA fw_version=%s remaining_cap=%dmAh full_cap=%dmAh charge=%d%% fault=0x%04x temp1=%d°C temp2=%d°C soh=%d%% low_soc=%v",
			r.data.Voltage, r.data.Current, r.data.FWVersion, r.data.RemainingCapacity, r.data.FullCapacity, r.data.Charge, r.data.FaultCode,
			r.data.Temperature[0], r.data.Temperature[1], r.data.StateOfHealth, r.data.LowSOC))

		time.Sleep(timeCmd)
		// Check HAL state again (must lock to access r.hal)
		r.nfcMutex.Lock()
		halState = r.hal.GetState()
		r.nfcMutex.Unlock()
		if halState != hal.StatePresent {
			lastMainError = fmt.Errorf("attempt %d: lost connection after reading Status0, HAL state: %s", attempt, halState)
			r.logCallback(hal.LogLevelWarning, lastMainError.Error())
			if attempt < maxAttemptsAfterHALRecreation {
				r.logCallback(hal.LogLevelWarning, "Attempting HAL recovery.")

				if err := r.simpleHALRecovery(); err != nil {
					return fmt.Errorf("%w; and HAL recovery failed: %v", lastMainError, err)
				}

				continue // Retry readBatteryStatus full sequence
			}
			return lastMainError
		}

		// Read Status1
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Attempt %d: Reading Status1...", attempt))
		data, err = r.readRegisterWithRetry(r.getOperationContext(), addrStatus1)
		if err != nil {
			lastMainError = fmt.Errorf("attempt %d: failed to read Status1: %w", attempt, err)
			
			// Don't retry HAL recovery for context cancelled errors (tag departed)
			if strings.Contains(err.Error(), "context cancel") {
				r.logCallback(hal.LogLevelDebug, "Status1 read cancelled due to tag departure, not retrying")
				return lastMainError
			}
			
			if errors.Is(err, errHALRecreatedRetryRead) && attempt < maxAttemptsAfterHALRecreation {
				r.logCallback(hal.LogLevelWarning, "HAL recreated during Status1 read, retrying entire status read.")
				continue // Retry the entire readBatteryStatus sequence
			}
			return lastMainError
		}
		// Process Status1 data
		r.data.State = BatteryState(uint32(data[3])<<24 | uint32(data[2])<<16 | uint32(data[1])<<8 | uint32(data[0]))
		copy(r.data.SerialNumber[0:12], data[4:16])

		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Status1 decoded: state=%s serial_number_part1=%s",
			r.data.State, string(r.data.SerialNumber[0:12])))

		time.Sleep(timeCmd)
		// Check HAL state again
		r.nfcMutex.Lock()
		halState = r.hal.GetState()
		r.nfcMutex.Unlock()
		if halState != hal.StatePresent {
			lastMainError = fmt.Errorf("attempt %d: lost connection after reading Status1, HAL state: %s", attempt, halState)
			r.logCallback(hal.LogLevelWarning, lastMainError.Error())
			if attempt < maxAttemptsAfterHALRecreation {
				r.logCallback(hal.LogLevelWarning, "Attempting HAL recovery.")

				if err := r.simpleHALRecovery(); err != nil {
					return fmt.Errorf("%w; and HAL recovery failed: %v", lastMainError, err)
				}

				continue // Retry readBatteryStatus full sequence
			}
			return lastMainError
		}

		// Read Status2
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Attempt %d: Reading Status2...", attempt))
		data, err = r.readRegisterWithRetry(r.getOperationContext(), addrStatus2)
		if err != nil {
			lastMainError = fmt.Errorf("attempt %d: failed to read Status2: %w", attempt, err)
			
			// Don't retry HAL recovery for context cancelled errors (tag departed)
			if strings.Contains(err.Error(), "context cancel") {
				r.logCallback(hal.LogLevelDebug, "Status2 read cancelled due to tag departure, not retrying")
				return lastMainError
			}
			
			if errors.Is(err, errHALRecreatedRetryRead) && attempt < maxAttemptsAfterHALRecreation {
				r.logCallback(hal.LogLevelWarning, "HAL recreated during Status2 read, retrying entire status read.")
				continue // Retry the entire readBatteryStatus sequence
			}
			return lastMainError
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

		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery status read complete (attempt %d).", attempt))

		// Check for low SOC state change and send event if needed
		r.dataMutex.RLock()
		previousLowSOC := r.lastPublishedData.LowSOC
		currentLowSOC := r.data.LowSOC
		r.dataMutex.RUnlock()

		if currentLowSOC && !previousLowSOC {
			r.logCallback(hal.LogLevelInfo, "Low SOC condition detected")
			r.stateMachine.SendEvent(EventLowSOC)
		}

		// Reset HAL reinit counter on successful status read
		r.dataMutex.Lock()
		r.halReinitCount = 0
		r.dataMutex.Unlock()
		
		// Update Redis with the new status
		if errUpdate := r.updateRedisStatus(); errUpdate != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after status read: %v", errUpdate))
			// Do not return error here, status was read, Redis is secondary for the success of this function itself.
		}
		return nil // Successfully read all statuses
	}

	// If loop finishes, it means all attempts failed.
	r.logCallback(hal.LogLevelError, fmt.Sprintf("Completely failed to read battery status after %d attempts. Last error: %v", maxAttemptsAfterHALRecreation, lastMainError))
	return fmt.Errorf("completely failed to read battery status after %d attempts: %w", maxAttemptsAfterHALRecreation, lastMainError)
}

// safelyPowerDownHAL safely powers down the HAL after ensuring no operations are in progress
func (r *BatteryReader) safelyPowerDownHAL() error {
	// Check vehicle state first - avoid power down in certain vehicle states
	r.service.Lock()
	vehicleState := r.service.vehicleState
	r.service.Unlock()

	// Only power down in stand-by state to avoid potential issues with other states
	if vehicleState != "stand-by" {
		r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Skipping HAL power down in '%s' vehicle state", vehicleState))
		return nil
	}

	// Log intent
	r.logCallback(hal.LogLevelInfo, "Preparing to power down HAL")

	// Set powered down flag first to prevent new operations
	r.dataMutex.Lock()
	wasAlreadyPoweredDown := r.isPoweredDown
	r.isPoweredDown = true
	r.dataMutex.Unlock()

	// If already powered down, no need to do it again
	if wasAlreadyPoweredDown {
		r.logCallback(hal.LogLevelInfo, "HAL was already powered down, no action needed")
		return nil
	}

	// Allow a short time for ongoing operations to complete
	time.Sleep(100 * time.Millisecond)

	// Acquire mutex to ensure exclusive access to HAL
	r.nfcMutex.Lock()
	defer r.nfcMutex.Unlock()

	// Deinitialize the HAL, catching any potential errors
	defer func() {
		if panicErr := recover(); panicErr != nil {
			r.logCallback(hal.LogLevelError, fmt.Sprintf("Recovered from panic during HAL deinitialization: %v", panicErr))
		}
	}()

	// Simply call deinitialize to power down
	currentState := r.hal.GetState()
	r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Deinitializing HAL (current state: %s)", currentState))
	r.hal.Deinitialize()
	r.logCallback(hal.LogLevelInfo, "HAL successfully powered down")

	return nil
}

// simpleHALRecovery performs HAL recovery using FullReinitialize to handle file descriptor issues
func (r *BatteryReader) simpleHALRecovery() error {
	r.logCallback(hal.LogLevelInfo, "Performing full HAL recovery with file descriptor renewal")

	// Increment HAL reinit counter
	r.dataMutex.Lock()
	r.halReinitCount++
	reinitCount := r.halReinitCount
	r.dataMutex.Unlock()
	
	r.logCallback(hal.LogLevelInfo, fmt.Sprintf("HAL reinit count: %d", reinitCount))

	// Always use FullReinitialize which properly handles file descriptor renewal
	// This is critical for proper recovery from 0300 errors and connection issues
	r.nfcMutex.Lock()
	errInit := r.hal.FullReinitialize()
	r.nfcMutex.Unlock()

	if errInit != nil {
		r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to reinitialize HAL during recovery: %v", errInit))
		return fmt.Errorf("failed to reinitialize HAL during recovery: %w", errInit)
	}

	r.logCallback(hal.LogLevelInfo, "Full HAL recovery completed successfully with file descriptor renewal")
	
	// Give the NFC hardware time to stabilize after reinitialization
	time.Sleep(100 * time.Millisecond)
	
	return nil
}
