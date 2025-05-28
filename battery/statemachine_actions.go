package battery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"battery-service/nfc/hal"
)

// Action methods for state transitions

func (sm *BatteryStateMachine) actionInitializeBattery(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Starting battery initialization sequence")

	// Initialize operation context for cancelling operations when tag departs
	sm.reader.Lock()
	if sm.reader.operationCancel != nil {
		sm.reader.operationCancel() // Cancel any existing context
	}
	sm.reader.operationCtx, sm.reader.operationCancel = context.WithCancel(sm.reader.service.ctx)
	isActiveBattery := sm.reader.index == 0
	sm.reader.Unlock()

	// First, inform battery it's in the scooter
	// Retry the InsertedInScooter command up to 3 times if we get 0300 errors
	sm.logger(hal.LogLevelDebug, "Sending InsertedInScooter command")
	insertedSent := false
	for attempt := 0; attempt < 3; attempt++ {
		if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandInsertedInScooter); err != nil {
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Attempt %d: Failed to send InsertedInScooter during init: %v", attempt+1, err))
			// If we get a 0300 error, the HAL will recover, wait a bit and retry
			time.Sleep(500 * time.Millisecond)
			continue
		}
		sm.logger(hal.LogLevelDebug, "InsertedInScooter command sent successfully")
		insertedSent = true
		time.Sleep(timeCmd) // Give battery time to process
		break
	}

	if !insertedSent {
		sm.logger(hal.LogLevelWarning, "Failed to send InsertedInScooter after 3 attempts, continuing anyway")
	}

	// For active battery (slot 0), skip initial status read to speed up activation
	if isActiveBattery {
		sm.logger(hal.LogLevelInfo, "Fast initialization for active battery - assuming idle state")
		
		// Set up battery data with assumed idle state
		sm.reader.Lock()
		sm.reader.readyToScoot = true
		sm.reader.justInserted = true
		sm.reader.data.Present = true
		sm.reader.data.State = BatteryStateIdle // Assume idle state for fast activation
		sm.reader.Unlock()

		// Send the ready event immediately
		go func() {
			time.Sleep(10 * time.Millisecond)
			sm.logger(hal.LogLevelDebug, "Sending EventReadyToScoot after fast initialization")
			sm.SendEvent(EventReadyToScoot)
		}()

		// Start background status read after initialization completes
		go func() {
			time.Sleep(500 * time.Millisecond) // Give state machine time to transition
			sm.logger(hal.LogLevelDebug, "Performing background status read after fast initialization")
			if err := sm.reader.readBatteryStatus(); err != nil {
				sm.logger(hal.LogLevelWarning, fmt.Sprintf("Background status read failed: %v", err))
			} else {
				sm.logger(hal.LogLevelInfo, "Background status read completed successfully")
			}
		}()

		return nil
	}

	// For inactive battery, do the full status read as before
	time.Sleep(500 * time.Millisecond)

	sm.logger(hal.LogLevelDebug, "Reading initial battery status")
	statusReadSuccess := false
	for attempt := 0; attempt < 3; attempt++ {
		if err := sm.reader.readBatteryStatus(); err != nil {
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Attempt %d: Status read failed: %v", attempt+1, err))
			// Wait progressively longer between retries
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		statusReadSuccess = true
		break
	}

	if !statusReadSuccess {
		sm.logger(hal.LogLevelError, "Failed to read battery status after 3 attempts")
		return fmt.Errorf("failed to read status after InsertedInScooter")
	}

	// If we got here, battery is responding properly
	sm.logger(hal.LogLevelInfo, "Battery status read successfully, setting up battery data")
	sm.reader.Lock()
	sm.reader.readyToScoot = true // Just set it true, don't wait for response
	sm.reader.justInserted = true
	sm.reader.data.Present = true
	batteryState := sm.reader.data.State
	sm.reader.Unlock()

	sm.logger(hal.LogLevelInfo, fmt.Sprintf("Battery initialization complete - state: %s", batteryState))

	// Send the ready event asynchronously to avoid deadlock
	go func() {
		time.Sleep(10 * time.Millisecond) // Small delay to ensure action completes
		sm.logger(hal.LogLevelDebug, "Sending EventReadyToScoot after initialization")
		sm.SendEvent(EventReadyToScoot)
	}()

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

	// For battery 0, always activate regardless of conditions
	if sm.reader.index == 0 {
		sm.logger(hal.LogLevelInfo, "Battery 0 initialization: triggering activation")
		// Schedule activation
		go func() {
			time.Sleep(timeCmd) // Use standard command delay
			if currentBatteryState == BatteryStateActive {
				// Battery is already active, send event to align state machine
				sm.SendEvent(EventBatteryAlreadyActive)
			} else {
				// Trigger activation
				sm.SendEvent(EventVehicleActive)
			}
		}()
	} else {
		// Battery 1 - check seatbox state
		sm.reader.service.Lock()
		seatboxOpen := sm.reader.service.seatboxOpen
		sm.reader.service.Unlock()
		
		if seatboxOpen {
			// Send event asynchronously
			go func() {
				time.Sleep(10 * time.Millisecond)
				sm.SendEvent(EventSeatboxOpened) // This will transition to IdleStandby
			}()
		}
	}

	return nil
}

func (sm *BatteryStateMachine) actionBatteryRemoved(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.Lock()
	sm.reader.data.Present = false
	sm.reader.readyToScoot = false
	sm.reader.justInserted = false
	sm.reader.Unlock()

	// Send BatteryRemoved command
	if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandBatteryRemoved); err != nil {
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
	if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandUserOpenedSeatbox); err != nil {
		return fmt.Errorf("failed to send UserOpenedSeatbox: %w", err)
	}
	return nil
}

func (sm *BatteryStateMachine) actionSeatboxClosed(machine *BatteryStateMachine, event BatteryEvent) error {
	// Send UserClosedSeatbox command
	if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandUserClosedSeatbox); err != nil {
		return fmt.Errorf("failed to send UserClosedSeatbox: %w", err)
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
		// Send event asynchronously to avoid deadlock
		go func() {
			time.Sleep(10 * time.Millisecond)
			sm.SendEvent(EventStateVerified)
		}()
		return nil
	}

	// Send activation commands immediately (not in goroutine for faster response)
	// First send UserClosedSeatbox if needed
	if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandUserClosedSeatbox); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send UserClosedSeatbox: %v", err))
		// Continue anyway - this is not critical
	}

	// Send ON command immediately
	if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandOn); err != nil {
		sm.logger(hal.LogLevelError, fmt.Sprintf("Failed to send ON command: %v", err))
		// Schedule failure event
		go func() {
			time.Sleep(10 * time.Millisecond)
			sm.SendEvent(EventCommandFailed)
		}()
		return fmt.Errorf("failed to send ON command: %w", err)
	}

	// Schedule state verification in background
	go func() {
		// Wait for battery to process command
		time.Sleep(timeStateVerify)

		if err := sm.reader.readBatteryStatus(); err != nil {
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to read status after ON command: %v", err))
			sm.SendEvent(EventStateVerificationFailed)
			return
		}

		sm.reader.Lock()
		newState := sm.reader.data.State
		sm.reader.Unlock()

		if newState == BatteryStateActive {
			sm.logger(hal.LogLevelInfo, "Battery activation verified successful")
			sm.SendEvent(EventStateVerified)
		} else {
			sm.logger(hal.LogLevelInfo, fmt.Sprintf("Battery state verification failed (%s), attempting recovery", newState))
			
			// Try recovery once
			if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandInsertedInScooter); err != nil {
				sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send recovery command: %v", err))
				sm.SendEvent(EventStateVerificationFailed)
				return
			}

			// Verify again after shorter delay
			time.Sleep(timeCmd)
			if err := sm.reader.readBatteryStatus(); err != nil {
				sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to read status after recovery: %v", err))
				sm.SendEvent(EventStateVerificationFailed)
				return
			}

			sm.reader.Lock()
			recoveryState := sm.reader.data.State
			sm.reader.Unlock()

			if recoveryState == BatteryStateActive {
				sm.logger(hal.LogLevelInfo, "Battery activation succeeded after recovery")
				sm.SendEvent(EventStateVerified)
			} else {
				sm.logger(hal.LogLevelWarning, fmt.Sprintf("Battery activation failed (state: %s)", recoveryState))
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
		// Send event asynchronously to avoid deadlock
		go func() {
			time.Sleep(10 * time.Millisecond)
			sm.SendEvent(EventStateVerified)
		}()
		return nil
	}

	// Send OFF command immediately
	if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandOff); err != nil {
		// Check if context was cancelled (tag departed)
		if strings.Contains(err.Error(), "context cancel") || strings.Contains(err.Error(), "invalid state") {
			sm.logger(hal.LogLevelInfo, "Deactivation command cancelled due to tag departure")
			// Don't send failure event - let tag departure handling take over
			return nil
		}
		// Schedule failure event for other errors
		go func() {
			time.Sleep(10 * time.Millisecond)
			sm.SendEvent(EventCommandFailed)
		}()
		return fmt.Errorf("failed to send OFF command: %w", err)
	}

	// Schedule state verification with shorter delay
	go func() {
		// Use operation context so this gets cancelled if tag departs
		ctx := sm.reader.getOperationContext()
		select {
		case <-time.After(timeStateVerify):
			// Continue with verification
		case <-ctx.Done():
			// Context cancelled, tag departed
			sm.logger(hal.LogLevelDebug, "Deactivation verification cancelled due to tag departure")
			return
		}

		if err := sm.reader.readBatteryStatus(); err != nil {
			// Check if context was cancelled
			if strings.Contains(err.Error(), "context cancel") {
				sm.logger(hal.LogLevelDebug, "Status read cancelled during deactivation due to tag departure")
				return
			}
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
		sm.logger(hal.LogLevelInfo, "Battery is active, need to align state machine")
		// Battery is active but state machine is in error state
		// Send event to transition directly to Active
		go func() {
			time.Sleep(10 * time.Millisecond)
			sm.SendEvent(EventBatteryAlreadyActive) // This will transition directly to Active
		}()
	}

	return nil
}

func (sm *BatteryStateMachine) actionBatteryAlreadyActive(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Battery is already active, transitioning state machine to Active")
	// Update Redis to reflect current state
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionHeartbeat(machine *BatteryStateMachine, event BatteryEvent) error {
	// Only battery 0 handles heartbeat logic
	if sm.reader.index != 0 {
		return nil
	}

	// Get current state information
	sm.reader.service.Lock()
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	currentState := sm.reader.data.State
	sm.reader.Unlock()

	// If seatbox is open, just send heartbeat
	if seatboxOpen {
		sm.logger(hal.LogLevelDebug, "Sending ScooterHeartbeat (Seatbox Open)")
		return sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandScooterHeartbeat)
	}

	// Seatbox is closed - send heartbeat and ensure battery stays on
	sm.logger(hal.LogLevelDebug, "Heartbeat tick: Seatbox closed")

	// Send UserClosedSeatbox command
	if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandUserClosedSeatbox); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send SEATBOX_CLOSED: %v", err))
	}
	time.Sleep(timeCmd)

	// Always ensure battery is ON
	if currentState != BatteryStateActive {
		sm.logger(hal.LogLevelInfo, "Battery not active, sending ON command")
		if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandOn); err != nil {
			return err
		}
	} else {
		// Battery is already active, just send heartbeat
		sm.logger(hal.LogLevelDebug, "Battery already active, sending heartbeat")
		if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandScooterHeartbeat); err != nil {
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send heartbeat: %v", err))
		}
	}

	return nil
}

func (sm *BatteryStateMachine) actionActiveStatusPoll(machine *BatteryStateMachine, event BatteryEvent) error {
	// Poll status for active battery
	if err := sm.reader.readBatteryStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to read battery status during active poll: %v", err))
		// Don't fail the transition - we'll retry on the next poll cycle
		// This prevents getting stuck after 0300 errors
		return nil
	}
	
	// Check if battery is still actually active after polling
	sm.reader.Lock()
	batteryState := sm.reader.data.State
	sm.reader.Unlock()
	
	if batteryState != BatteryStateActive && sm.reader.IsActive() {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Active battery %d is not active after status poll (state: %s), reactivating", sm.reader.index, batteryState))
		// For active batteries, always reactivate
		if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandOn); err != nil {
			sm.logger(hal.LogLevelError, fmt.Sprintf("Failed to reactivate battery: %v", err))
		}
	}
	
	return nil
}

func (sm *BatteryStateMachine) actionStartMaintenance(machine *BatteryStateMachine, event BatteryEvent) error {
	// Start maintenance cycle for idle batteries
	sm.logger(hal.LogLevelDebug, "Starting maintenance cycle")
	return nil
}

func (sm *BatteryStateMachine) actionMaintenanceComplete(machine *BatteryStateMachine, event BatteryEvent) error {
	// Complete maintenance cycle
	sm.logger(hal.LogLevelDebug, "Maintenance cycle complete")
	if err := sm.reader.readBatteryStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to read battery status during maintenance: %v", err))
		// Don't fail the transition - we'll retry on the next maintenance cycle
		// The important thing is to exit Maintenance state so we don't get stuck
		return nil
	}
	
	// After reading status, check if battery state has changed and we need to sync state machine
	sm.reader.Lock()
	batteryState := sm.reader.data.State
	sm.reader.Unlock()
	
	// If battery is active after maintenance, we need to transition to Active state
	if batteryState == BatteryStateActive {
		sm.logger(hal.LogLevelInfo, "Battery is active after maintenance, syncing state machine")
		go func() {
			time.Sleep(10 * time.Millisecond)
			// The maintenance will transition to IdleStandby, then we send the event
			sm.SendEvent(EventBatteryAlreadyActive)
		}()
	}
	
	return nil
}

func (sm *BatteryStateMachine) actionMaintenanceCompleteWithActivation(machine *BatteryStateMachine, event BatteryEvent) error {
	// Complete maintenance cycle and check if activation is needed
	sm.logger(hal.LogLevelInfo, "Vehicle became active during maintenance, completing maintenance and checking activation")
	
	// First complete the maintenance read if possible
	if err := sm.reader.readBatteryStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to read battery status during maintenance: %v", err))
		// Continue anyway - we need to handle the vehicle state change
	}
	
	// Update Redis status
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after maintenance: %v", err))
	}
	
	// After transitioning to IdleStandby, immediately check if we should activate
	go func() {
		time.Sleep(50 * time.Millisecond) // Small delay to ensure state transition completes
		
		// Check current conditions
		sm.reader.service.Lock()
		seatboxOpen := sm.reader.service.seatboxOpen
		sm.reader.service.Unlock()
		
		sm.reader.Lock()
		ready := sm.reader.readyToScoot
		enabled := sm.reader.enabled
		batteryState := sm.reader.data.State
		index := sm.reader.index
		sm.reader.Unlock()
		
		// Only battery 0 can be activated
		if index == 0 && enabled && ready && !seatboxOpen {
			
			sm.logger(hal.LogLevelInfo, fmt.Sprintf("Vehicle active after maintenance, triggering activation (batteryState=%s)", batteryState))
			
			// If battery is already active, sync state machine
			if batteryState == BatteryStateActive {
				sm.SendEvent(EventBatteryAlreadyActive)
			} else {
				// Trigger activation through proper state transitions
				sm.SendEvent(EventVehicleActive)
			}
		}
	}()
	
	return nil
}

func (sm *BatteryStateMachine) actionLowSOC(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelWarning, "Battery SOC is low")
	return sm.reader.updateRedisStatus()
}


func (sm *BatteryStateMachine) actionVehicleActive(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "Vehicle became active - ensuring battery is on")

	// Only battery 0 can be activated
	if sm.reader.index != 0 {
		return nil
	}

	sm.reader.service.Lock()
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	sm.reader.Unlock()

	// Always activate if enabled, ready, and seatbox closed (ignore low SOC)
	if enabled && ready && !seatboxOpen {
		sm.logger(hal.LogLevelInfo, "Vehicle active - triggering battery activation")
		// Already handled by state transition
	} else {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("Vehicle active but cannot activate: enabled=%v, ready=%v, lowSOC=%v, seatboxOpen=%v", enabled, ready, lowSOC, seatboxOpen))
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
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	batteryPresent := sm.reader.data.Present
	sm.reader.Unlock()

	// Always enable battery 0 if it's present
	if batteryPresent {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 (present=%v, seatboxOpen=%v)", batteryPresent, seatboxOpen))

		// Enable the battery
		sm.reader.Lock()
		sm.reader.enabled = true
		sm.reader.Unlock()

		// Send EventEnabled to trigger proper initialization and activation sequence
		go func() {
			// Small delay to ensure state transition completes first
			time.Sleep(10 * time.Millisecond)
			sm.logger(hal.LogLevelDebug, "Sending EventEnabled after vehicle active")
			sm.SendEvent(EventEnabled)
		}()

		sm.logger(hal.LogLevelInfo, "Battery reader enabled")
		return nil // Transition to StateNotPresent
	}

	sm.logger(hal.LogLevelDebug, fmt.Sprintf("Battery 0 cannot be enabled (present=%v, seatboxOpen=%v)", batteryPresent, seatboxOpen))
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
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.Lock()
	batteryPresent := sm.reader.data.Present
	sm.reader.Unlock()

	// Always enable battery 0 if it's present
	if batteryPresent {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 (present=%v, seatboxOpen=%v)", batteryPresent, seatboxOpen))

		// Enable the battery
		sm.reader.Lock()
		sm.reader.enabled = true
		sm.reader.Unlock()

		// Schedule a battery insertion event to trigger initialization
		go func() {
			// Small delay to ensure state transition completes first
			time.Sleep(10 * time.Millisecond)
			sm.logger(hal.LogLevelDebug, "Sending EventBatteryInserted after enabling")
			sm.SendEvent(EventBatteryInserted)
		}()

		sm.logger(hal.LogLevelInfo, "Battery reader enabled")
		return nil // Transition to StateNotPresent
	} else {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("Battery 0 cannot be enabled (present=%v, seatboxOpen=%v)", batteryPresent, seatboxOpen))
		// Don't enable, stay in disabled state
		return fmt.Errorf("battery activation conditions not met")
	}
}

func (sm *BatteryStateMachine) actionLowSOCWhileActive(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelWarning, "Battery SOC is low while active - keeping battery on")

	// Update Redis status with low SOC condition
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after low SOC: %v", err))
	}

	// Keep the battery active regardless of low SOC
	sm.logger(hal.LogLevelInfo, "Keeping battery active despite low SOC")

	return nil
}

func (sm *BatteryStateMachine) actionStartDiscovery(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Tag departed - cancelling pending operations and starting discovery")
	
	// Cancel any pending operations immediately since tag is gone
	sm.reader.Lock()
	if sm.reader.operationCancel != nil {
		sm.logger(hal.LogLevelDebug, "Cancelling pending operations due to tag departure")
		sm.reader.operationCancel()
	}
	// Create new operation context for future operations
	sm.reader.operationCtx, sm.reader.operationCancel = context.WithCancel(sm.reader.service.ctx)
	sm.reader.Unlock()
	
	// Start a timer for discovery timeout (250ms initial discovery attempt)
	go func() {
		time.Sleep(timeDeparture) // 250ms initial discovery attempt
		sm.SendEvent(EventDiscoveryTimeout)
	}()
	
	return nil
}

func (sm *BatteryStateMachine) actionStartWaitingForArrival(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "Discovery timed out - waiting for arrival")
	
	go func() {
		time.Sleep(timeDeparture) // Another 250ms wait
		sm.SendEvent(EventDiscoveryTimeout)
	}()
	
	return nil
}

func (sm *BatteryStateMachine) actionHandleDeparture(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Tag not found after timeout - handling departure")
	
	sm.reader.Lock()
	sm.reader.data.Present = false
	sm.reader.justInserted = false
	sm.reader.readyToScoot = false
	sm.reader.Unlock()
	
	// Update Redis
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after departure: %v", err))
	}
	
	// Send battery removed command
	sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandBatteryRemoved)
	
	return nil
}


