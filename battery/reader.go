package battery

import (
	"errors"
	"fmt"
	"time"

	"battery-service/nfc/hal"
)

// Start starts the battery reader
func (r *BatteryReader) Start() error {
	// Enable the reader by default
	r.enabled = true
	r.batteryRemovedThreshold = 5          // At least 5 consecutive absences before battery is considered removed
	r.isPoweredDown = false                // Ensure we start with the reader powered up
	r.zeroTagDetections = 0                // Initialize zero tag detection counter
	r.consecutiveZeroInitializingCount = 0 // Initialize counter for zero detections while initializing

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

	close(r.stopChan)

	// Reset powered down state to ensure final deinitialization works
	r.Lock()
	wasPoweredDown := r.isPoweredDown
	r.isPoweredDown = false
	r.Unlock()

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
	r.Lock()
	r.enabled = enabled
	r.Unlock()

	// Send appropriate event to state machine
	if enabled {
		r.stateMachine.SendEvent(EventEnabled)
	} else {
		r.stateMachine.SendEvent(EventDisabled)
	}
}

// monitorTags continuously monitors for tag presence
func (r *BatteryReader) monitorTags() {
	// Initialize battery state as not present
	r.Lock()
	if !r.data.Present {
		r.data = BatteryData{}       // Ensure clean state
		r.lastPublishedData = r.data // Initialize lastPublishedData to prevent false-positive changes on first update
		if err := r.updateRedisStatus(); err != nil {
			r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to initialize Redis status: %v", err))
		}
	}
	r.Unlock()

	// Default poll period in milliseconds
	const defaultPollPeriod uint = 500

	for {
		select {
		case <-r.stopChan:
			return
		default:
			// Check if the reader is temporarily powered down
			r.Lock()
			isPoweredDown := r.isPoweredDown
			r.Unlock()

			if isPoweredDown {
				// Skip tag detection when powered down, use longer sleep to reduce CPU usage
				time.Sleep(1 * time.Second)
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

				// Reset zero tag counter when we find a tag
				r.Lock()
				r.zeroTagDetections = 0
				r.Unlock()
			} else if len(tags) > 1 {
				r.logCallback(hal.LogLevelWarning, "Multiple tags detected, ignoring")
				r.handleTagAbsent() // Handles state change and Redis update

				// Reset zero tag counter when we find multiple tags
				r.Lock()
				r.zeroTagDetections = 0
				r.Unlock()
			} else {
				// No tags detected
				r.handleTagAbsent() // Handles state change and Redis update

				// Increment zero tag counter and check if we need to restart discovery
				r.Lock()
				r.zeroTagDetections++
				zeroTagCount := r.zeroTagDetections
				r.Unlock()

				// Every 5 consecutive zero detections, restart discovery
				if zeroTagCount >= 5 {
					r.logCallback(hal.LogLevelInfo, "5 consecutive zero tag detections - restarting discovery")

					r.nfcMutex.Lock()
					// Get current HAL state
					halState := r.hal.GetState()

					// Only attempt to restart discovery if HAL is in a valid state
					if halState == hal.StateInitializing {
						r.logCallback(hal.LogLevelWarning, "Cannot restart discovery - HAL is still initializing")

						// If initialization has been stuck for some time, try hardware reset
						if r.consecutiveZeroInitializingCount > 3 {
							r.logCallback(hal.LogLevelWarning, "HAL stuck in initializing state - attempting hardware reset")
							r.consecutiveZeroInitializingCount = 0
							r.nfcMutex.Unlock() // Release mutex before reset

							// Use simple approach to reset the hardware
							if err := r.forceHardwareReset(); err != nil {
								r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to reset hardware: %v", err))
							}
						} else {
							r.consecutiveZeroInitializingCount++
							r.nfcMutex.Unlock()
						}

						// Reset counter to avoid continuous logging
						r.Lock()
						r.zeroTagDetections = 1
						r.Unlock()
					} else if halState != hal.StateUninitialized && halState != hal.StateInitializing {
						// Stop discovery first
						_ = r.hal.StopDiscovery() // Ignore errors, we'll try to restart anyway

						// Start discovery again
						if err := r.hal.StartDiscovery(defaultPollPeriod); err != nil {
							r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Failed to restart discovery: %v", err))
						} else {
							r.logCallback(hal.LogLevelInfo, "Successfully restarted discovery")
						}
						r.nfcMutex.Unlock()

						// Reset counter
						r.Lock()
						r.zeroTagDetections = 0
						r.Unlock()
					} else {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Cannot restart discovery - HAL in state: %s", halState))
						r.nfcMutex.Unlock()

						// Use simple HAL recovery approach
						r.logCallback(hal.LogLevelInfo, "Attempting to recover from uninitialized state")
						if err := r.simpleHALRecovery(); err != nil {
							r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed HAL recovery during uninitialized recovery: %v", err))
						}

						// Reset counter
						r.Lock()
						r.zeroTagDetections = 0
						r.Unlock()
					}
				}
			}

			// Always use a short sleep interval to ensure responsiveness for absence detection
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// startHeartbeat starts the heartbeat goroutine
func (r *BatteryReader) startHeartbeat() {
	go func() {
		ticker := time.NewTicker(timeHeartbeatIntervalScooter)
		defer ticker.Stop()
		var lastStatusPoll time.Time   // Track time of last status poll when active
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
				shouldPollStatus := r.index == 0 && present && state == BatteryStateActive && time.Since(lastStatusPoll) >= timeActiveStatusPoll // Only for battery 0
				r.Unlock()                                                                                                                       // Release lock before potentially blocking IO

				if present {
					if seatboxOpen {
						// --- Seatbox OPEN Logic ---
						if r.index == 0 { // Only send heartbeats for battery 0
							r.logCallback(hal.LogLevelDebug, "Battery 0: Sending ScooterHeartbeat (Seatbox Open: true)")
							if err := r.sendCommand(r.service.ctx, BatteryCommandScooterHeartbeat); err != nil {
								r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery 0: Failed to send SCOOTER_HEARTBEAT: %v", err))
							}
						} else {
							r.logCallback(hal.LogLevelDebug, "Battery 1: Skipping ScooterHeartbeat (Seatbox Open: true)")
						}
					} else {
						// --- Seatbox CLOSED Logic (Implements diagram's heartbeat substate) ---
						if r.index == 0 {
							r.logCallback(hal.LogLevelDebug, "Battery 0: Heartbeat tick: Running closed seatbox maintenance cycle")

							// Determine if UserClosedSeatbox should be skipped
							skipUserClosedSeatbox := false
							r.service.Lock()
							vehicleState := r.service.vehicleState
							cbCharge := r.service.cbBatteryCharge
							r.service.Unlock()

							r.Lock()
							currentState := r.data.State
							r.Unlock()

							if vehicleState == "stand-by" && cbCharge >= cbBatteryActivationThreshold &&
								(currentState == BatteryStateIdle || currentState == BatteryStateAsleep) {
								r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0: Expecting idle/asleep (state: %s, vehicle: stand-by, cb-charge: %d%% >= %d%%). Skipping UserClosedSeatbox.", currentState, cbCharge, cbBatteryActivationThreshold))
								skipUserClosedSeatbox = true
							}

							if !skipUserClosedSeatbox {
								// 1. Send SEATBOX_CLOSED
								if err := r.sendCommand(r.service.ctx, BatteryCommandUserClosedSeatbox); err != nil {
									r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery 0: Failed to send SEATBOX_CLOSED in maint: %v", err))
									// Optional: goto nextIteration if this fails?
								}
								time.Sleep(timeCmd) // tm_closed equivalent
							} else {
								// If UserClosedSeatbox is skipped, there's no specific delay needed here before ON/OFF determination.
								r.logCallback(hal.LogLevelDebug, "Battery 0: UserClosedSeatbox command skipped.")
							}

							// 2. Determine and Send ON/OFF Command
							_, expectedState, err := r.determineAndSendCommandOnOff()
							if err != nil {
								r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery 0: Failed to determine/send ON/OFF command in maint: %v", err))
								// Optional: goto nextIteration if this fails?
							}

							time.Sleep(timeCmd) // tm_on_off equivalent

							// Check if we should poll less frequently for battery 0
							pollLessFrequently := false
							if vehicleState == "stand-by" { // vehicleState and cbCharge were fetched for skipUserClosedSeatbox logic
								if cbCharge >= cbBatteryActivationThreshold && (expectedState == BatteryStateIdle || expectedState == BatteryStateAsleep) {
									r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0: Stand-by mode, cb-charge (%d%%) allows idle/asleep. Expected state: %s. Will check for infrequent poll.", cbCharge, expectedState))
									pollLessFrequently = true
								}
							}

							if pollLessFrequently {
								if time.Since(r.lastIdleAsleepPollTime) >= timeMaintPollIdleAsleepInterval {
									r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0: Time for infrequent idle/asleep poll (interval: %s)", timeMaintPollIdleAsleepInterval))

									// Power up the reader if needed before polling
									r.Lock()
									if r.isPoweredDown {
										r.logCallback(hal.LogLevelInfo, "Powering up HAL before low-frequency poll")
										r.isPoweredDown = false
										r.Unlock()

										// Use simple HAL recovery approach
										if err := r.simpleHALRecovery(); err != nil {
											r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed HAL recovery before low-frequency poll: %v", err))
											// Skip this polling cycle if we can't recover HAL
											r.lastIdleAsleepPollTime = time.Now()
											continue
										}

										r.logCallback(hal.LogLevelInfo, "HAL powered up and initialized successfully for polling")
									} else {
										r.Unlock()
									}

									// Check state after powering up the HAL
									if !r.checkStateCorrectAfterRead(expectedState) {
										// State is incorrect, attempt recovery
										r.logCallback(hal.LogLevelWarning, "Battery 0: State incorrect after ON/OFF (infrequent poll), attempting recovery...")

										// Use simple HAL recovery approach
										if err := r.simpleHALRecovery(); err != nil {
											r.logCallback(hal.LogLevelError, fmt.Sprintf("Battery 0: HAL recovery failed: %v", err))
										} else {
											r.logCallback(hal.LogLevelInfo, "Battery 0: HAL recovery completed successfully")
										}
									} else {
										r.logCallback(hal.LogLevelDebug, "Battery 0: Idle/asleep state correct (infrequent poll), waiting for next infrequent poll cycle.")
									}

									r.lastIdleAsleepPollTime = time.Now()

									// Power down the reader after polling until next cycle
									r.Lock()
									if !r.isPoweredDown && r.data.Present && (r.data.State == BatteryStateIdle || r.data.State == BatteryStateAsleep) {
										r.logCallback(hal.LogLevelInfo, "Powering down HAL until next low-frequency polling cycle")
										r.Unlock()

										// Safely deinitialize the HAL
										if err := r.safelyPowerDownHAL(); err != nil {
											r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to power down HAL: %v", err))
										}
									} else {
										r.Unlock()
									}
								} else {
									r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0: Expecting idle/asleep, waiting for next infrequent poll interval (last poll: %s, next in: %s)", r.lastIdleAsleepPollTime.Format(time.RFC3339), r.lastIdleAsleepPollTime.Add(timeMaintPollIdleAsleepInterval).Sub(time.Now()).Round(time.Second)))
								}
							} else {
								// 3. Read status and check state correctness (regular frequency)
								if !r.checkStateCorrectAfterRead(expectedState) {
									// State is incorrect, attempt recovery
									r.logCallback(hal.LogLevelWarning, "Battery 0: State incorrect after ON/OFF – performing HAL recovery")

									if err := r.simpleHALRecovery(); err != nil {
										r.logCallback(hal.LogLevelError, fmt.Sprintf("Battery 0: HAL recovery failed during maint: %v", err))
									} else {
										r.logCallback(hal.LogLevelInfo, "Battery 0: HAL recovery completed successfully")
									}
								} else {
									// State is correct, wait for next tick (wait_update equivalent)
									r.logCallback(hal.LogLevelDebug, "Battery 0: State correct, waiting for next maint cycle.")
								}
							}
						} else if r.index == 1 {
							// --- Battery 1 Seatbox CLOSED Logic (Infrequent Maintenance Poll) ---
							if time.Since(r.lastBattery1MaintPollTime) >= timeBattery1MaintPollInterval {
								r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 1: Time for infrequent maintenance poll (interval: %s)", timeBattery1MaintPollInterval))

								// 1. Send SEATBOX_CLOSED (still good for battery to know seatbox is closed at time of poll)
								if err := r.sendCommand(r.service.ctx, BatteryCommandUserClosedSeatbox); err != nil {
									r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery 1: Failed to send SEATBOX_CLOSED in maint poll: %v", err))
									// Continue to attempt status read even if this fails for now
								}

								time.Sleep(timeCmd) // tm_closed equivalent, give battery time to process command

								// 2. Read status
								if err := r.readBatteryStatus(); err != nil {
									r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery 1: Error during infrequent status poll: %v", err))
								} else {
									r.logCallback(hal.LogLevelDebug, "Battery 1: Infrequent status poll successful.")
								}
								r.lastBattery1MaintPollTime = time.Now()
							} else {
								// Not time to poll battery 1 yet, log for debugging if needed (can be verbose)
								// r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 1: Seatbox closed, waiting for next infrequent poll interval (last poll: %s, next in: %s)", r.lastBattery1MaintPollTime.Format(time.RFC3339), r.lastBattery1MaintPollTime.Add(timeBattery1MaintPollInterval).Sub(time.Now()).Round(time.Second)))
							}
						}
					}
				}

				// Separately, handle periodic status polling IF ACTIVE (for battery 0 only)
				if shouldPollStatus { // condition now includes r.index == 0
					r.logCallback(hal.LogLevelDebug, "Polling active battery 0 status due to interval")
					lastStatusPoll = time.Now()
					if err := r.readBatteryStatus(); err != nil {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Error during periodic status poll: %v", err))
					}
				}

				// Additionally, poll battery 0 every 10 seconds regardless of state, if present,
				// UNLESS it's in stand-by, expected idle/asleep, and covered by the 2-minute maintenance poll.
				r.Lock()
				isBattery0_GP := r.index == 0 // Suffix GP for General Poll scope
				present_GP := r.data.Present
				currentState_GP := r.data.State
				r.Unlock()

				if isBattery0_GP && present_GP && time.Since(lastBattery0Poll) >= timeActiveStatusPoll {
					// This 10s poll interval is up. Decide if we should proceed or defer.

					// First, check if it was just polled by the 'active' poll (which runs if state is BatteryStateActive)
					// 'shouldPollStatus' is determined earlier in the heartbeat loop based on active state.
					if shouldPollStatus && time.Since(lastStatusPoll) <= 1*time.Second { // Using 1s buffer as before
						lastBattery0Poll = lastStatusPoll // Align timers if active poll just ran
						r.logCallback(hal.LogLevelDebug, "Battery 0: General 10s poll slot skipped (active poll just ran).")
					} else {
						// Not recently polled by 'active' poll. Now check stand-by idle/asleep conditions.
						r.service.Lock()
						vehicleState_GP := r.service.vehicleState
						cbCharge_GP := r.service.cbBatteryCharge
						r.service.Unlock()

						isStandbyIdleAsleep := vehicleState_GP == "stand-by" &&
							cbCharge_GP >= cbBatteryActivationThreshold &&
							(currentState_GP == BatteryStateIdle || currentState_GP == BatteryStateAsleep)

						if isStandbyIdleAsleep {
							// Relying on the 2-minute poll from the maintenance cycle, so this 10s status read is skipped.
							// Update lastBattery0Poll to effectively "consume" this 10s slot and prevent log spam / re-evaluation next tick.
							lastBattery0Poll = time.Now()
							r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0: General 10s status read deferred (vehicle: %s, cb-charge: %d%%, state: %s). Using 2-min maint poll.", vehicleState_GP, cbCharge_GP, currentState_GP))
						} else {
							// Conditions for deferral NOT met, so proceed with the 10s general poll.
							r.logCallback(hal.LogLevelDebug, "Battery 0: Polling status due to 10s general interval.")
							lastBattery0Poll = time.Now()
							if err := r.readBatteryStatus(); err != nil {
								r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery 0: Error during 10s general periodic poll: %v", err))
							}
						}
					}
				}
			}
		}
	}()
}

// handleTagPresent handles a present tag
func (r *BatteryReader) handleTagPresent() {
	r.Lock()
	// Reset the consecutive absence counter when tag is detected
	r.consecutiveTagAbsences = 0

	if !r.data.Present {
		r.data.Present = true // Mark as present immediately
		r.Unlock()

		r.logCallback(hal.LogLevelInfo, "Battery inserted")
		// Send battery insertion event to state machine
		r.stateMachine.SendEvent(EventBatteryInserted)
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

	r.Unlock() // Unlock as we are done with reader data for now
}

// handleTagAbsent handles an absent tag
func (r *BatteryReader) handleTagAbsent() {
	r.Lock()
	defer r.Unlock()

	// Check if battery was previously present
	if r.data.Present {
		// Simple counter approach: increment consecutive absences
		r.consecutiveTagAbsences++

		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Tag absence detected (%d/%d)",
			r.consecutiveTagAbsences, r.batteryRemovedThreshold))

		// Only mark as removed if consistently absent for the threshold period
		if r.consecutiveTagAbsences >= r.batteryRemovedThreshold {
			r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery removed after %d consecutive absences", r.consecutiveTagAbsences))

			// Send battery removal event to state machine
			r.stateMachine.SendEvent(EventBatteryRemoved)
		}
	}
}

// readBatteryStatus reads the battery status registers
func (r *BatteryReader) readBatteryStatus() error {
	r.logCallback(hal.LogLevelDebug, "Attempting to read battery status...")

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
		data, err = r.readRegisterWithRetry(r.service.ctx, addrStatus0)
		if err != nil {
			lastMainError = fmt.Errorf("attempt %d: failed to read Status0: %w", attempt, err)
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
		data, err = r.readRegisterWithRetry(r.service.ctx, addrStatus1)
		if err != nil {
			lastMainError = fmt.Errorf("attempt %d: failed to read Status1: %w", attempt, err)
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
		data, err = r.readRegisterWithRetry(r.service.ctx, addrStatus2)
		if err != nil {
			lastMainError = fmt.Errorf("attempt %d: failed to read Status2: %w", attempt, err)
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
	r.Lock()
	wasAlreadyPoweredDown := r.isPoweredDown
	r.isPoweredDown = true
	r.Unlock()

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

// forceHardwareReset performs hardware reset using simple approach
func (r *BatteryReader) forceHardwareReset() error {
	r.logCallback(hal.LogLevelWarning, "Performing hardware reset using simple approach")

	// First mark as not powered down
	r.Lock()
	r.isPoweredDown = false
	r.Unlock()

	return r.simpleHALRecovery()
}

// simpleHALRecovery performs HAL recovery using FullReinitialize to handle file descriptor issues
func (r *BatteryReader) simpleHALRecovery() error {
	r.logCallback(hal.LogLevelInfo, "Performing HAL recovery with file descriptor renewal")

	// Use FullReinitialize which properly handles file descriptor renewal
	// This is critical when "bad file descriptor" errors occur
	r.nfcMutex.Lock()
	errInit := r.hal.FullReinitialize()
	r.nfcMutex.Unlock()

	if errInit != nil {
		r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to reinitialize HAL during recovery: %v", errInit))
		return fmt.Errorf("failed to reinitialize HAL during recovery: %w", errInit)
	}

	r.logCallback(hal.LogLevelInfo, "HAL recovery completed successfully with file descriptor renewal")
	return nil
}

// createNewHAL creates a new HAL instance using simple deinitialize/initialize approach
func (r *BatteryReader) createNewHAL() error {
	return r.simpleHALRecovery()
}
