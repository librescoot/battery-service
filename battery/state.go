package battery

import (
	"fmt"
	"time"

	"battery-service/nfc/hal"
)

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

// handleSeatboxStateChange is called when the seatbox state changes
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

		// For Battery 0 only: If ready and not already active, check conditions before sending ON command
		if r.index == 0 && ready && state != BatteryStateActive {
			// Check vehicle state and CB battery charge to decide whether to activate
			r.service.Lock()
			vehicleState := r.service.vehicleState
			cbCharge := r.service.cbBatteryCharge
			r.service.Unlock()

			// Only activate if not in standby mode or CB charge is below threshold
			if vehicleState != "stand-by" || (cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold) {
				r.logCallback(hal.LogLevelInfo, "Seatbox closed, battery 0 ready. Sending ON command.")
				// Call activateBattery which now just sends ON
				r.activateBattery()
			} else {
				r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Seatbox closed, battery 0 ready, but in stand-by mode with CB charge %d%%. Skipping activation.", cbCharge))
			}
		}
	}
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
	enabled := r.enabled
	r.Unlock()

	if !enabled {
		r.logCallback(hal.LogLevelDebug, "activateBattery: reader disabled, skipping ON command.")
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

	// Check cb-battery condition if vehicle is in stand-by for battery 0
	r.service.Lock()
	vehicleState := r.service.vehicleState
	cbCharge := r.service.cbBatteryCharge
	r.service.Unlock()

	// Conditions where OFF is desired
	shouldForceOff := false
	if !enabled || lowSOC {
		shouldForceOff = true
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for OFF met (enabled=%v, lowSOC=%v)", enabled, lowSOC))
	}

	// Additional condition for OFF for battery 0 if in stand-by and cb-charge is high
	if index == 0 && vehicleState == "stand-by" && cbCharge >= cbBatteryDeactivationThreshold {
		if currentState == BatteryStateActive { // Only force OFF if active
			shouldForceOff = true
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for OFF met for Battery 0 (stand-by, cb-charge %d%% >= %d%%)", cbCharge, cbBatteryDeactivationThreshold))
		} else {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0 (stand-by, cb-charge %d%% >= %d%%) but not Active (state: %s). OFF not forced by this rule.", cbCharge, cbBatteryDeactivationThreshold, currentState))
		}
	}

	if shouldForceOff {
		if currentState != BatteryStateAsleep && currentState != BatteryStateIdle {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Sending OFF. Current state=%s", currentState))
			sentCmd = BatteryCommandOff
			expectedState = BatteryStateIdle // Or Asleep? Idle seems more likely after OFF cmd
			err = r.sendCommand(r.service.ctx, sentCmd)
		} else {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("OFF condition met but already Asleep/Idle. Expecting %s.", currentState))
			expectedState = currentState // Already in a low power state
			err = nil                    // No command sent, no error
		}
		return sentCmd, expectedState, err
	}

	// Conditions where ON is desired (only for battery 0)
	if index == 0 && ready {
		// Check cb-battery level if in stand-by
		if vehicleState == "stand-by" {
			if cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold {
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0 ON condition (stand-by, cb-charge %d%% < %d%%).", cbCharge, cbBatteryActivationThreshold))
				// Proceed to check current state for ON
			} else if cbCharge >= cbBatteryActivationThreshold {
				r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery 0 ON suppressed (stand-by, cb-charge %d%% >= %d%%). Current state: %s", cbCharge, cbBatteryActivationThreshold, currentState))
				expectedState = currentState       // No ON command, expect current state
				err = nil                          // No command sent, no error
				return sentCmd, expectedState, err // Don't send ON
			} else { // cbCharge < 0 (unknown)
				r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery 0 ON suppressed (stand-by, cb-charge unknown %d%%). Current state: %s", cbCharge, currentState))
				expectedState = currentState
				err = nil
				return sentCmd, expectedState, err // Don't send ON
			}
		}
		// If not in stand-by, or if in stand-by and cb-charge allows ON:
		if currentState != BatteryStateActive {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for ON met (index=%d, ready=%v, vehicleState=%s, cbCharge=%d), current state=%s. Sending ON.", index, ready, vehicleState, cbCharge, currentState))
			sentCmd = BatteryCommandOn
			expectedState = BatteryStateActive
			err = r.sendCommand(r.service.ctx, sentCmd)
		} else {
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for ON met but already Active (index=%d, ready=%v). Expecting Active.", index, ready))
			expectedState = BatteryStateActive // Already active
			err = nil                          // No command sent, no error
		}
		return sentCmd, expectedState, err
	}

	// Default case: No command needed (e.g., battery 1 is present and ready, or battery 0 not ready, or conditions not met for ON/OFF)
	r.logCallback(hal.LogLevelDebug, fmt.Sprintf("No ON/OFF command needed (index=%d, ready=%v, state=%s, vehicleState=%s, cbCharge=%d). Expecting %s.", index, ready, currentState, vehicleState, cbCharge, expectedState))
	err = nil // No command sent, no error
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

// manageBattery0PowerState checks conditions and sends ON/OFF commands to battery 0
// based on vehicle state and cb-battery charge.
func (s *Service) manageBattery0PowerState() {
	s.Lock() // Lock to read service-level state: vehicleState, cbBatteryCharge
	vehicleState := s.vehicleState
	cbCharge := s.cbBatteryCharge
	reader0 := s.readers[0]
	s.Unlock()

	if reader0 == nil {
		s.logger.Printf("[PowerMgmtB0] Battery 0 reader not available.")
		return
	}

	reader0.Lock() // Lock to read reader-specific state
	present := reader0.data.Present
	currentState := reader0.data.State
	readyToScoot := reader0.readyToScoot // Important for sending ON
	enabled := reader0.enabled           // Ensure reader is enabled
	reader0.Unlock()

	if !present || !enabled {
		s.logger.Printf("[PowerMgmtB0] Battery 0 not present (%v) or reader not enabled (%v). No action.", present, enabled)
		return
	}

	if vehicleState == "stand-by" {
		s.logger.Printf("[PowerMgmtB0] Vehicle in stand-by. CB-Charge: %d%%. Battery 0 State: %s", cbCharge, currentState)
		if cbCharge < 0 {
			s.logger.Printf("[PowerMgmtB0] CB-Battery charge unknown (%d). No action.", cbCharge)
			return
		}

		if cbCharge < cbBatteryActivationThreshold {
			// Activate if not already Active and is ready
			if currentState != BatteryStateActive && readyToScoot {
				s.logger.Printf("[PowerMgmtB0] CB-Charge %d%% < %d%%. Activating Battery 0.", cbCharge, cbBatteryActivationThreshold)
				if err := reader0.sendCommand(s.ctx, BatteryCommandOn); err != nil {
					s.logger.Printf("[PowerMgmtB0] Error sending ON command to Battery 0: %v", err)
				} else {
					// Optionally, verify state change after a delay, or let heartbeat/status poll handle it
					// For now, assume command is processed. Subsequent status reads will reflect new state.
				}
			} else if currentState == BatteryStateActive {
				s.logger.Printf("[PowerMgmtB0] CB-Charge %d%% < %d%%. Battery 0 already active.", cbCharge, cbBatteryActivationThreshold)
			} else if !readyToScoot {
				s.logger.Printf("[PowerMgmtB0] CB-Charge %d%% < %d%%. Battery 0 not ready to scoot, cannot send ON.", cbCharge, cbBatteryActivationThreshold)
			}
		} else if cbCharge >= cbBatteryDeactivationThreshold {
			// Deactivate if currently Active
			if currentState == BatteryStateActive {
				s.logger.Printf("[PowerMgmtB0] CB-Charge %d%% >= %d%%. Deactivating Battery 0.", cbCharge, cbBatteryDeactivationThreshold)
				if err := reader0.sendCommand(s.ctx, BatteryCommandOff); err != nil {
					s.logger.Printf("[PowerMgmtB0] Error sending OFF command to Battery 0: %v", err)
				} else {
					// Similar to ON, assume command processed. Subsequent status reads will reflect.
				}
			} else {
				s.logger.Printf("[PowerMgmtB0] CB-Charge %d%% >= %d%%. Battery 0 not active (state: %s). No OFF command needed.", cbCharge, cbBatteryDeactivationThreshold, currentState)
			}
		} else {
			s.logger.Printf("[PowerMgmtB0] CB-Charge %d%% is between thresholds (%d%% - %d%%). No change to Battery 0 power state.", cbCharge, cbBatteryActivationThreshold, cbBatteryDeactivationThreshold)
		}
	} else {
		s.logger.Printf("[PowerMgmtB0] Vehicle not in stand-by (state: %s). CB-battery based control inactive.", vehicleState)
	}
}
