package battery

import (
	"fmt"

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
	r.Unlock()

	if !present {
		return // Ignore if battery not present
	}

	// Send the appropriate event to the state machine
	if isOpen {
		r.logCallback(hal.LogLevelInfo, "Seatbox opened")
		r.stateMachine.SendEvent(EventSeatboxOpened)
	} else {
		r.logCallback(hal.LogLevelInfo, "Seatbox closed")
		r.stateMachine.SendEvent(EventSeatboxClosed)
	}
}

// activateBattery is now handled by the state machine
func (r *BatteryReader) activateBattery() {
	// This method is now deprecated - the state machine handles activation automatically
	// Send an event to trigger activation through the state machine
	r.logCallback(hal.LogLevelDebug, "activateBattery called - delegating to state machine")
	r.stateMachine.SendEvent(EventSeatboxClosed)
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

	// Check cb-battery and aux-battery condition if vehicle is in stand-by for battery 0
	r.service.Lock()
	vehicleState := r.service.vehicleState
	cbCharge := r.service.cbBatteryCharge
	auxVoltage := r.service.auxBatteryVoltage
	r.service.Unlock()

	// Conditions where OFF is desired
	shouldForceOff := false
	if !enabled || lowSOC {
		shouldForceOff = true
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for OFF met (enabled=%v, lowSOC=%v)", enabled, lowSOC))
	}

	// Additional condition for OFF for battery 0 if in stand-by and BOTH cb-charge is high AND aux-voltage is high
	if index == 0 && vehicleState == "stand-by" {
		cbChargeOk := cbCharge < 0 || cbCharge >= cbBatteryDeactivationThreshold
		auxVoltageOk := auxVoltage < 0 || auxVoltage >= auxBatteryDeactivationThreshold
		
		if cbChargeOk && auxVoltageOk {
			if currentState == BatteryStateActive { // Only force OFF if active
				shouldForceOff = true
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Condition for OFF met for Battery 0 (stand-by, cb-charge %d%% >= %d%%, aux-voltage %dmV >= %dmV)", cbCharge, cbBatteryDeactivationThreshold, auxVoltage, auxBatteryDeactivationThreshold))
			} else {
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0 (stand-by, cb-charge %d%% >= %d%%, aux-voltage %dmV >= %dmV) but not Active (state: %s). OFF not forced by this rule.", cbCharge, cbBatteryDeactivationThreshold, auxVoltage, auxBatteryDeactivationThreshold, currentState))
			}
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
		// Check cb-battery and aux-battery levels if in stand-by
		if vehicleState == "stand-by" {
			shouldTurnOn := false
			onReason := ""
			
			// Check cb-battery
			if cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold {
				shouldTurnOn = true
				onReason = fmt.Sprintf("cb-charge %d%% < %d%%", cbCharge, cbBatteryActivationThreshold)
			}
			
			// Check aux-battery
			if auxVoltage >= 0 && auxVoltage < auxBatteryActivationThreshold {
				shouldTurnOn = true
				if onReason != "" {
					onReason += " OR "
				}
				onReason += fmt.Sprintf("aux-voltage %dmV < %dmV", auxVoltage, auxBatteryActivationThreshold)
			}
			
			if shouldTurnOn {
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0 ON condition (stand-by, %s).", onReason))
				// Proceed to check current state for ON
			} else {
				r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery 0 ON suppressed (stand-by, cb-charge %d%%, aux-voltage %dmV). Current state: %s", cbCharge, auxVoltage, currentState))
				expectedState = currentState       // No ON command, expect current state
				err = nil                          // No command sent, no error
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
	r.logCallback(hal.LogLevelDebug, fmt.Sprintf("No ON/OFF command needed (index=%d, ready=%v, state=%s, vehicleState=%s, cbCharge=%d, auxVoltage=%d). Expecting %s.", index, ready, currentState, vehicleState, cbCharge, auxVoltage, expectedState))
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
// based on vehicle state, cb-battery charge, and aux-battery voltage.
func (s *Service) manageBattery0PowerState() {
	s.Lock() // Lock to read service-level state: vehicleState, cbBatteryCharge, auxBatteryVoltage
	vehicleState := s.vehicleState
	cbCharge := s.cbBatteryCharge
	auxVoltage := s.auxBatteryVoltage
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
		s.logger.Printf("[PowerMgmtB0] Vehicle in stand-by. CB-Charge: %d%%, Aux-Voltage: %dmV. Battery 0 State: %s", cbCharge, auxVoltage, currentState)
		
		// Check if we need to activate battery 0
		shouldActivate := false
		activationReason := ""
		
		// Check cb-battery charge
		if cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold {
			shouldActivate = true
			activationReason = fmt.Sprintf("CB-Charge %d%% < %d%%", cbCharge, cbBatteryActivationThreshold)
		}
		
		// Check aux-battery voltage
		if auxVoltage >= 0 && auxVoltage < auxBatteryActivationThreshold {
			shouldActivate = true
			if activationReason != "" {
				activationReason += " AND "
			}
			activationReason += fmt.Sprintf("Aux-Voltage %dmV < %dmV", auxVoltage, auxBatteryActivationThreshold)
		}
		
		if shouldActivate {
			// Activate if not already Active and is ready
			if currentState != BatteryStateActive && readyToScoot {
				s.logger.Printf("[PowerMgmtB0] %s. Activating Battery 0.", activationReason)
				if err := reader0.sendCommand(s.ctx, BatteryCommandOn); err != nil {
					s.logger.Printf("[PowerMgmtB0] Error sending ON command to Battery 0: %v", err)
				}
			} else if currentState == BatteryStateActive {
				s.logger.Printf("[PowerMgmtB0] %s. Battery 0 already active.", activationReason)
			} else if !readyToScoot {
				s.logger.Printf("[PowerMgmtB0] %s. Battery 0 not ready to scoot, cannot send ON.", activationReason)
			}
		} else {
			// Check if we should deactivate battery 0
			// Only deactivate if BOTH cb-battery is charged AND aux-battery has good voltage
			cbChargeOk := cbCharge < 0 || cbCharge >= cbBatteryDeactivationThreshold
			auxVoltageOk := auxVoltage < 0 || auxVoltage >= auxBatteryDeactivationThreshold
			
			if cbChargeOk && auxVoltageOk {
				// Deactivate if currently Active
				if currentState == BatteryStateActive {
					deactivationReason := ""
					if cbCharge >= 0 {
						deactivationReason = fmt.Sprintf("CB-Charge %d%% >= %d%%", cbCharge, cbBatteryDeactivationThreshold)
					}
					if auxVoltage >= 0 {
						if deactivationReason != "" {
							deactivationReason += " AND "
						}
						deactivationReason += fmt.Sprintf("Aux-Voltage %dmV >= %dmV", auxVoltage, auxBatteryDeactivationThreshold)
					}
					s.logger.Printf("[PowerMgmtB0] %s. Deactivating Battery 0.", deactivationReason)
					if err := reader0.sendCommand(s.ctx, BatteryCommandOff); err != nil {
						s.logger.Printf("[PowerMgmtB0] Error sending OFF command to Battery 0: %v", err)
					}
				} else {
					s.logger.Printf("[PowerMgmtB0] Both batteries OK (CB: %d%%, Aux: %dmV). Battery 0 not active (state: %s). No OFF command needed.", cbCharge, auxVoltage, currentState)
				}
			} else {
				s.logger.Printf("[PowerMgmtB0] No action needed. CB-Charge: %d%% (threshold: %d%%), Aux-Voltage: %dmV (threshold: %dmV)", cbCharge, cbBatteryActivationThreshold, auxVoltage, auxBatteryActivationThreshold)
			}
		}
	} else {
		s.logger.Printf("[PowerMgmtB0] Vehicle not in stand-by (state: %s). CB-battery and aux-battery based control inactive.", vehicleState)
	}
}
