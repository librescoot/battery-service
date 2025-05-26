package battery

import (
	"fmt"
	"time"

	"battery-service/nfc/hal"
)

// Action methods for state transitions

func (sm *BatteryStateMachine) actionInitializeBattery(machine *BatteryStateMachine, event BatteryEvent) error {
	// Read initial status
	if err := sm.reader.readBatteryStatus(); err != nil {
		// If we can't read status, send InsertedInScooter as recovery
		if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandInsertedInScooter); err != nil {
			return fmt.Errorf("failed to send InsertedInScooter: %w", err)
		}
		time.Sleep(10 * time.Second) // Wait like C code does

		// Try reading status again
		if err := sm.reader.readBatteryStatus(); err != nil {
			return fmt.Errorf("failed to read status after InsertedInScooter: %w", err)
		}
	}

	// If we got here, battery is responding properly
	sm.reader.Lock()
	sm.reader.readyToScoot = true // Just set it true, don't wait for response
	sm.reader.justInserted = true
	sm.reader.data.Present = true
	sm.reader.Unlock()

	// Send the ready event
	sm.SendEvent(EventReadyToScoot)

	return nil
}

func (sm *BatteryStateMachine) actionBatteryReady(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.readyToScoot = true
	sm.reader.justInserted = false
	// Don't force the state to idle - keep the actual battery state that was read
	currentBatteryState := sm.reader.data.State
	sm.reader.Unlock()

	// Update Redis with initial state
	if err := sm.reader.updateRedisStatus(); err != nil {
		return fmt.Errorf("failed to update Redis: %w", err)
	}

	// Check initial conditions to determine state machine state based on actual battery state
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	// If battery is already active, we need to transition to Active state
	if currentBatteryState == BatteryStateActive {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Battery is already active during initialization, transitioning to Active state"))
		// Schedule transition to Active state
		go func() {
			time.Sleep(10 * time.Millisecond)
			// Need to go through proper state transitions
			if seatboxOpen || (vehicleState == "stand-by" && cbCharge >= cbBatteryActivationThreshold) {
				// First go to standby, then to active
				sm.SendEvent(EventSeatboxOpened) // This will transition to IdleStandby
				time.Sleep(10 * time.Millisecond)
				// Force transition to Active state since battery is already active
				sm.Lock()
				sm.currentState = StateActive
				sm.lastStateChange = time.Now()
				sm.Unlock()
				sm.logger(hal.LogLevelInfo, "State changed to Active")
			} else {
				// Go directly to ActiveRequested then Active
				sm.SendEvent(EventSeatboxClosed) // This will transition to ActiveRequested
				time.Sleep(10 * time.Millisecond)
				sm.SendEvent(EventStateVerified) // This will transition to Active
			}
		}()
	} else {
		// Battery is idle/asleep, follow normal flow
		if seatboxOpen || (vehicleState == "stand-by" && cbCharge >= cbBatteryActivationThreshold) {
			sm.SendEvent(EventSeatboxOpened) // This will transition to IdleStandby
		}
	}

	return nil
}

func (sm *BatteryStateMachine) actionBatteryRemoved(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Present = false
	sm.reader.readyToScoot = false
	sm.reader.justInserted = false
	sm.reader.consecutiveTagAbsences = 0
	sm.reader.Unlock()

	// Send BatteryRemoved command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandBatteryRemoved); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send BatteryRemoved command: %v", err))
	}

	// Update Redis
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after battery removal: %v", err))
	}

	return nil
}

func (sm *BatteryStateMachine) actionSeatboxOpened(machine *BatteryStateMachine, event BatteryEvent) error {
	// Send UserOpenedSeatbox command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandUserOpenedSeatbox); err != nil {
		return fmt.Errorf("failed to send UserOpenedSeatbox: %w", err)
	}
	return nil
}

func (sm *BatteryStateMachine) actionSeatboxClosed(machine *BatteryStateMachine, event BatteryEvent) error {
	// Send UserClosedSeatbox command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandUserClosedSeatbox); err != nil {
		return fmt.Errorf("failed to send UserClosedSeatbox: %w", err)
	}
	return nil
}

func (sm *BatteryStateMachine) actionCheckActivationConditions(machine *BatteryStateMachine, event BatteryEvent) error {
	// Check if battery should be activated based on current conditions
	if sm.reader.index != 0 {
		return nil // Only battery 0 can be activated
	}

	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	sm.reader.Unlock()

	shouldActivate := enabled && ready && !lowSOC && !seatboxOpen &&
		(vehicleState != "stand-by" || (cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold))

	if shouldActivate {
		sm.SendEvent(EventSeatboxClosed) // This will trigger activation
	}

	return nil
}

func (sm *BatteryStateMachine) actionRequestActivation(machine *BatteryStateMachine, event BatteryEvent) error {
	// Only battery 0 can be activated
	if sm.reader.index != 0 {
		return nil
	}

	// Check pre-conditions
	sm.reader.Lock()
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	state := sm.reader.data.State
	sm.reader.Unlock()

	if !enabled || !ready || lowSOC {
		return fmt.Errorf("activation preconditions not met: enabled=%v, ready=%v, lowSOC=%v", enabled, ready, lowSOC)
	}

	if state == BatteryStateActive {
		sm.SendEvent(EventStateVerified)
		return nil
	}

	// First send UserClosedSeatbox to ensure battery knows seatbox is closed
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandUserClosedSeatbox); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send UserClosedSeatbox before ON: %v", err))
		// Continue anyway - this is not critical
	} else {
		time.Sleep(timeCmd) // Give battery time to process
	}

	// Send ON command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandOn); err != nil {
		sm.SendEvent(EventCommandFailed)
		return fmt.Errorf("failed to send ON command: %w", err)
	}

	// Schedule state verification
	go func() {
		time.Sleep(timeStateVerify)
		if err := sm.reader.readBatteryStatus(); err != nil {
			sm.SendEvent(EventStateVerificationFailed)
		} else {
			sm.reader.Lock()
			newState := sm.reader.data.State
			sm.reader.Unlock()

			if newState == BatteryStateActive {
				sm.SendEvent(EventStateVerified)
			} else {
				sm.SendEvent(EventStateVerificationFailed)
			}
		}
	}()

	return nil
}

func (sm *BatteryStateMachine) actionRequestDeactivation(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	state := sm.reader.data.State
	sm.reader.Unlock()

	if state == BatteryStateIdle || state == BatteryStateAsleep {
		sm.SendEvent(EventStateVerified)
		return nil
	}

	// Send OFF command
	if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandOff); err != nil {
		sm.SendEvent(EventCommandFailed)
		return fmt.Errorf("failed to send OFF command: %w", err)
	}

	// Schedule state verification
	go func() {
		time.Sleep(timeStateVerify)
		if err := sm.reader.readBatteryStatus(); err != nil {
			sm.SendEvent(EventStateVerificationFailed)
		} else {
			sm.reader.Lock()
			newState := sm.reader.data.State
			sm.reader.Unlock()

			if newState == BatteryStateIdle || newState == BatteryStateAsleep {
				sm.SendEvent(EventStateVerified)
			} else {
				sm.SendEvent(EventStateVerificationFailed)
			}
		}
	}()

	return nil
}

func (sm *BatteryStateMachine) actionActivationSuccess(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Battery successfully activated")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionDeactivationSuccess(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Battery successfully deactivated")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionActivationFailed(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.NotFollowingCommand = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelError, "Battery activation failed")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionDeactivationFailed(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.NotFollowingCommand = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelError, "Battery deactivation failed")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionHeartbeatInError(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "Heartbeat in error state - attempting recovery")
	
	// Read current battery status to check actual state
	if err := sm.reader.readBatteryStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to read status in error recovery: %v", err))
		return err
	}
	
	// Check if battery is actually in a good state now
	sm.reader.Lock()
	batteryState := sm.reader.data.State
	present := sm.reader.data.Present
	sm.reader.Unlock()
	
	if present && (batteryState == BatteryStateIdle || batteryState == BatteryStateAsleep) {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Battery recovered to %s state, transitioning out of error", batteryState))
		// Send recovery event
		go func() {
			time.Sleep(10 * time.Millisecond)
			sm.SendEvent(EventHALRecovered)
		}()
	} else if present && batteryState == BatteryStateActive {
		sm.logger(hal.LogLevelInfo, "Battery is active, transitioning to active state")
		// Transition directly to active
		go func() {
			time.Sleep(10 * time.Millisecond)
			sm.currentState = StateActive
			sm.lastStateChange = time.Now()
		}()
	}
	
	return nil
}

func (sm *BatteryStateMachine) actionHeartbeat(machine *BatteryStateMachine, event BatteryEvent) error {
	// Only battery 0 handles heartbeat logic
	if sm.reader.index != 0 {
		return nil
	}

	// Get current state information
	sm.reader.service.Lock()
	seatboxOpen := sm.reader.service.seatboxOpen
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	sm.reader.service.Unlock()

	sm.reader.Lock()
	currentState := sm.reader.data.State
	sm.reader.Unlock()

	// If seatbox is open, just send heartbeat
	if seatboxOpen {
		sm.logger(hal.LogLevelDebug, "Sending ScooterHeartbeat (Seatbox Open)")
		return sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandScooterHeartbeat)
	}

	// Seatbox is closed - evaluate battery control conditions
	sm.logger(hal.LogLevelDebug, "Heartbeat tick: Evaluating battery control conditions")

	// Check if we should skip UserClosedSeatbox command
	skipUserClosedSeatbox := false
	if vehicleState == "stand-by" && cbCharge >= cbBatteryActivationThreshold &&
		(currentState == BatteryStateIdle || currentState == BatteryStateAsleep) {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("Expecting idle/asleep (state: %s, vehicle: stand-by, cb-charge: %d%% >= %d%%). Skipping UserClosedSeatbox.", currentState, cbCharge, cbBatteryActivationThreshold))
		skipUserClosedSeatbox = true
	}

	if !skipUserClosedSeatbox {
		if err := sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandUserClosedSeatbox); err != nil {
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send SEATBOX_CLOSED: %v", err))
		}
		time.Sleep(timeCmd)
	}

	// Now evaluate what state the battery should be in
	return sm.evaluateBatteryControl()
}

func (sm *BatteryStateMachine) actionActiveStatusPoll(machine *BatteryStateMachine, event BatteryEvent) error {
	// Poll status for active battery
	return sm.reader.readBatteryStatus()
}

func (sm *BatteryStateMachine) actionStartMaintenance(machine *BatteryStateMachine, event BatteryEvent) error {
	// Start maintenance cycle for idle batteries
	sm.logger(hal.LogLevelDebug, "Starting maintenance cycle")
	return nil
}

func (sm *BatteryStateMachine) actionMaintenanceComplete(machine *BatteryStateMachine, event BatteryEvent) error {
	// Complete maintenance cycle
	sm.logger(hal.LogLevelDebug, "Maintenance cycle complete")
	return sm.reader.readBatteryStatus()
}

func (sm *BatteryStateMachine) actionLowSOC(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelWarning, "Battery SOC is low")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionCBChargeHigh(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "CB battery charge is high, entering standby mode")
	return nil
}

func (sm *BatteryStateMachine) actionVehicleStandby(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "Vehicle entered standby mode")
	return nil
}

func (sm *BatteryStateMachine) actionVehicleActive(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "Vehicle became active (non-standby) - checking activation conditions")

	// Check if battery should be activated based on current conditions
	if sm.reader.index != 0 {
		return nil // Only battery 0 can be activated
	}

	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	sm.reader.Unlock()

	shouldActivate := enabled && ready && !lowSOC && !seatboxOpen &&
		(vehicleState != "stand-by" || (cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold))

	if shouldActivate {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Vehicle active conditions met for Battery 0 activation: vehicleState=%s, enabled=%v, ready=%v, lowSOC=%v, seatboxOpen=%v", vehicleState, enabled, ready, lowSOC, seatboxOpen))
		sm.SendEvent(EventSeatboxClosed) // This will trigger activation through StateIdleReady -> StateActiveRequested
	} else {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("Vehicle active but activation conditions not met: vehicleState=%s, enabled=%v, ready=%v, lowSOC=%v, seatboxOpen=%v, cbCharge=%d", vehicleState, enabled, ready, lowSOC, seatboxOpen, cbCharge))
	}

	return nil
}

func (sm *BatteryStateMachine) actionCommandFailed(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.CommunicationError = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelError, "Battery command failed")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionHALError(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.ReaderError = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelError, "HAL error occurred")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionRecovery(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Faults.ReaderError = false
	sm.reader.data.Faults.CommunicationError = false
	sm.reader.data.Faults.NotFollowingCommand = false
	sm.reader.Unlock()

	sm.logger(hal.LogLevelInfo, "Recovery from error state")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionDisable(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.enabled = false
	sm.reader.Unlock()

	sm.logger(hal.LogLevelInfo, "Battery reader disabled")
	return nil
}

func (sm *BatteryStateMachine) actionEnable(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.enabled = true
	sm.reader.Unlock()

	sm.logger(hal.LogLevelInfo, "Battery reader enabled")
	return nil
}

func (sm *BatteryStateMachine) actionVehicleActiveWhileDisabled(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Vehicle became active while battery disabled - checking if battery should be enabled")

	// Check if this is battery 0 (only battery 0 can be activated)
	if sm.reader.index != 0 {
		sm.logger(hal.LogLevelDebug, "Battery 1 remains disabled (only battery 0 can be activated)")
		return fmt.Errorf("battery 1 should not be enabled")
	}

	// Get current conditions
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	batteryPresent := sm.reader.data.Present
	sm.reader.Unlock()

	// In parked mode, we should always enable battery 0 if it's present
	if vehicleState == "parked" && batteryPresent {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 due to parked state (present=%v, seatboxOpen=%v)", batteryPresent, seatboxOpen))

		// Enable the battery
		sm.reader.Lock()
		sm.reader.enabled = true
		sm.reader.Unlock()

		// Send EventEnabled to trigger proper initialization
		go func() {
			// Small delay to ensure state transition completes first
			time.Sleep(10 * time.Millisecond)
			sm.logger(hal.LogLevelDebug, "Sending EventEnabled after vehicle active")
			sm.SendEvent(EventEnabled)
		}()

		sm.logger(hal.LogLevelInfo, "Battery reader enabled")
		return nil // Transition to StateNotPresent
	}

	// For non-parked states, check normal activation conditions
	shouldEnable := batteryPresent && !seatboxOpen &&
		(vehicleState != "stand-by" || (cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold))

	if shouldEnable {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 due to vehicle state '%s' (present=%v, seatboxOpen=%v, cbCharge=%d)", vehicleState, batteryPresent, seatboxOpen, cbCharge))

		// Enable the battery
		sm.reader.Lock()
		sm.reader.enabled = true
		sm.reader.Unlock()

		// Send EventEnabled to trigger proper initialization
		go func() {
			// Small delay to ensure state transition completes first
			time.Sleep(10 * time.Millisecond)
			sm.logger(hal.LogLevelDebug, "Sending EventEnabled after vehicle active")
			sm.SendEvent(EventEnabled)
		}()

		sm.logger(hal.LogLevelInfo, "Battery reader enabled")
		return nil // Transition to StateNotPresent
	}

	sm.logger(hal.LogLevelDebug, fmt.Sprintf("Battery 0 activation conditions not met (vehicleState=%s, present=%v, seatboxOpen=%v, cbCharge=%d)", vehicleState, batteryPresent, seatboxOpen, cbCharge))
	return fmt.Errorf("battery activation conditions not met")
}

func (sm *BatteryStateMachine) actionBatteryInsertedWhileDisabled(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Battery inserted while disabled - checking if battery should be enabled")

	// Check if this is battery 0 (only battery 0 can be activated)
	if sm.reader.index != 0 {
		sm.logger(hal.LogLevelDebug, "Battery 1 remains disabled (only battery 0 can be activated)")
		// For battery 1, we don't enable it, so we should stay in disabled state
		// Return an error to prevent the transition to StateNotPresent
		return fmt.Errorf("battery 1 should not be enabled")
	}

	// Get current conditions
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	batteryPresent := sm.reader.data.Present
	sm.reader.Unlock()

	// Check if conditions suggest the battery should be enabled
	// Enable for non-standby states (like "parked") when battery is present
	shouldEnable := batteryPresent && !seatboxOpen &&
		(vehicleState != "stand-by" || (cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold))

	if shouldEnable {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 due to vehicle state '%s' (present=%v, seatboxOpen=%v, cbCharge=%d)", vehicleState, batteryPresent, seatboxOpen, cbCharge))

		// Enable the battery (this action sets enabled=true like actionEnable)
		sm.reader.Lock()
		sm.reader.enabled = true
		sm.reader.Unlock()

		// If battery is present, schedule a battery insertion event to trigger initialization
		if batteryPresent {
			go func() {
				// Small delay to ensure state transition completes first
				time.Sleep(10 * time.Millisecond)
				sm.logger(hal.LogLevelDebug, "Sending EventBatteryInserted after enabling")
				sm.SendEvent(EventBatteryInserted)
			}()
		}

		sm.logger(hal.LogLevelInfo, "Battery reader enabled")
		return nil // Transition to StateNotPresent
	} else {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("Battery 0 activation conditions not met (vehicleState=%s, present=%v, seatboxOpen=%v, cbCharge=%d)", vehicleState, batteryPresent, seatboxOpen, cbCharge))
		// Don't enable, stay in disabled state
		return fmt.Errorf("battery activation conditions not met")
	}
}

func (sm *BatteryStateMachine) actionLowSOCWhileActive(machine *BatteryStateMachine, event BatteryEvent) error {
	// Get current vehicle state to determine if we should deactivate
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	sm.reader.service.Unlock()

	sm.logger(hal.LogLevelWarning, fmt.Sprintf("Battery SOC is low while active (vehicleState=%s)", vehicleState))

	// Update Redis status with low SOC condition
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after low SOC: %v", err))
	}

	// Only deactivate if vehicle is in standby mode
	// During rides or in park mode, keep the battery active despite low SOC
	if vehicleState == "stand-by" {
		sm.logger(hal.LogLevelInfo, "Vehicle is in standby mode - deactivating battery due to low SOC")
		// Trigger deactivation by sending an event that will transition to StateDeactivating
		go func() {
			// Small delay to ensure current state transition completes
			time.Sleep(10 * time.Millisecond)
			sm.SendEvent(EventVehicleStandby)
		}()
	} else {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Vehicle is in %s mode - keeping battery active despite low SOC", vehicleState))
	}

	return nil
}

// actionCheckDeactivationConditions checks if conditions are met for battery deactivation
// This ensures the battery only turns off in standby mode when both batteries are charged
func (sm *BatteryStateMachine) actionCheckDeactivationConditions(machine *BatteryStateMachine, event BatteryEvent) error {
	// Get current state information
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	auxVoltage := sm.reader.service.auxBatteryVoltage
	sm.reader.service.Unlock()

	sm.reader.Lock()
	currentState := sm.reader.data.State
	sm.reader.Unlock()

	// Only allow deactivation in standby mode
	if vehicleState != "stand-by" {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Ignoring %s event - vehicle not in standby (state: %s)", event, vehicleState))
		return nil
	}

	// Check if both batteries are sufficiently charged
	cbChargeOk := cbCharge < 0 || cbCharge >= cbBatteryDeactivationThreshold
	auxVoltageOk := auxVoltage < 0 || auxVoltage >= auxBatteryDeactivationThreshold

	if cbChargeOk && auxVoltageOk && currentState == BatteryStateActive {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Deactivation conditions met in standby (cb: %d%%, aux: %dmV)", cbCharge, auxVoltage))
		// Transition to deactivating
		go func() {
			time.Sleep(10 * time.Millisecond)
			sm.SendEvent(EventSeatboxOpened) // Reuse this event to trigger deactivation
		}()
	} else {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("Deactivation conditions not met (cb: %d%%, aux: %dmV, cbOk: %v, auxOk: %v)",
			cbCharge, auxVoltage, cbChargeOk, auxVoltageOk))
	}

	return nil
}

// evaluateBatteryControl evaluates current conditions and sends appropriate commands
// This replaces the old determineAndSendCommandOnOff function
func (sm *BatteryStateMachine) evaluateBatteryControl() error {
	// Get all state information
	sm.reader.service.Lock()
	vehicleState := sm.reader.service.vehicleState
	cbCharge := sm.reader.service.cbBatteryCharge
	auxVoltage := sm.reader.service.auxBatteryVoltage
	sm.reader.service.Unlock()

	sm.reader.Lock()
	currentState := sm.reader.data.State
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	index := sm.reader.index
	sm.reader.Unlock()

	// Only battery 0 has power management logic
	if index != 0 {
		return nil
	}

	// Determine if battery should be OFF
	shouldBeOff := false
	offReason := ""

	// Basic conditions for OFF
	if !enabled {
		shouldBeOff = true
		offReason = "disabled"
	} else if lowSOC && vehicleState == "stand-by" {
		// Only turn off for low SOC if in standby
		shouldBeOff = true
		offReason = "low SOC in standby"
	}

	// Check aux/cb battery conditions in standby
	if vehicleState == "stand-by" && !shouldBeOff {
		cbChargeOk := cbCharge < 0 || cbCharge >= cbBatteryDeactivationThreshold
		auxVoltageOk := auxVoltage < 0 || auxVoltage >= auxBatteryDeactivationThreshold

		if cbChargeOk && auxVoltageOk {
			shouldBeOff = true
			offReason = fmt.Sprintf("batteries charged (cb: %d%%, aux: %dmV)", cbCharge, auxVoltage)
		}
	}

	// Determine if battery should be ON
	shouldBeOn := false
	onReason := ""

	if enabled && ready && !lowSOC && !shouldBeOff {
		if vehicleState == "stand-by" {
			// Check if either battery needs charging
			if cbCharge >= 0 && cbCharge < cbBatteryActivationThreshold {
				shouldBeOn = true
				onReason = fmt.Sprintf("cb-battery low (%d%%)", cbCharge)
			}
			if auxVoltage >= 0 && auxVoltage < auxBatteryActivationThreshold {
				shouldBeOn = true
				if onReason != "" {
					onReason += " and "
				}
				onReason += fmt.Sprintf("aux-battery low (%dmV)", auxVoltage)
			}
		} else {
			// Not in standby - turn on if not already
			shouldBeOn = true
			onReason = fmt.Sprintf("vehicle %s", vehicleState)
		}
	}

	// Execute appropriate command
	if shouldBeOff && currentState == BatteryStateActive {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Turning battery OFF: %s (current state: %s)", offReason, currentState))
		
		// If we're already in Active state, we need to trigger the deactivation process
		// Send EventSeatboxOpened which will transition to StateDeactivating
		if sm.currentState == StateActive {
			sm.logger(hal.LogLevelDebug, "Triggering deactivation from Active state")
			go func() {
				time.Sleep(10 * time.Millisecond)
				sm.SendEvent(EventSeatboxOpened)
			}()
			return nil
		}
		
		// Otherwise send OFF command directly
		return sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandOff)
	} else if shouldBeOn && currentState != BatteryStateActive {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Turning battery ON: %s", onReason))
		return sm.reader.sendCommand(sm.reader.service.ctx, BatteryCommandOn)
	}

	sm.logger(hal.LogLevelDebug, fmt.Sprintf("No action needed (state: %s, vehicle: %s, cb: %d%%, aux: %dmV)",
		currentState, vehicleState, cbCharge, auxVoltage))
	return nil
}
