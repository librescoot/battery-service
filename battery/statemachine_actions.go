package battery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"battery-service/nfc/hal"
)

// Action methods for state transitions

// handleCmdError interprets a command error. If it's a departure-like error,
// it sends the EventTagDeparted and returns true. Otherwise, it returns false.
func (sm *BatteryStateMachine) handleCmdError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// This list checks for various I/O errors that all indicate the tag is no longer reachable.
	if strings.Contains(errStr, "tag departed") ||
		strings.Contains(errStr, "discovering state") ||
		strings.Contains(errStr, "invalid state for writing") ||
		strings.Contains(errStr, "invalid state for reading") ||
		strings.Contains(errStr, "lost connection") ||
		strings.Contains(errStr, "no such device or address") ||
		strings.Contains(errStr, "No response after credit notification") ||
		strings.Contains(errStr, "context cancel") {

		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Command failed, assuming tag departure. Error: %v", err))
		go func() {
			time.Sleep(timeCmd)
			sm.SendEvent(EventTagDeparted)
		}()
		return true // Error was handled as a departure.
	}
	return false // Error is not a departure.
}

func (sm *BatteryStateMachine) actionInitializeBattery(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelInfo, "Starting battery initialization sequence")

	// Initialize operation context for cancelling operations when tag departs
	sm.reader.dataMutex.Lock()
	if sm.reader.operationCancel != nil {
		sm.reader.operationCancel() // Cancel any existing context
	}
	sm.reader.operationCtx, sm.reader.operationCancel = context.WithCancel(sm.reader.service.ctx)
	isActiveBattery := sm.reader.role == BatteryRoleActive
	sm.reader.dataMutex.Unlock()

	// First, inform battery it's in the scooter
	// Retry the InsertedInScooter command up to 3 times if we get 0300 errors
	sm.logger(hal.LogLevelDebug, "Sending InsertedInScooter command")
	insertedSent := false
	for attempt := 0; attempt < 3; attempt++ {
		if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandInsertedInScooter); err != nil {
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Attempt %d: Failed to send InsertedInScooter during init: %v", attempt+1, err))
			// If we get a 0300 error, the HAL will recover, wait a bit and retry
			time.Sleep(timeReinit)
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
		sm.reader.dataMutex.Lock()
		sm.reader.readyToScoot = true
		sm.reader.justInserted = true
		sm.reader.data.Present = true
		sm.reader.data.State = BatteryStateIdle // Assume idle state for fast activation
		sm.reader.dataMutex.Unlock()

		// Send the ready event immediately
		go func() {
			time.Sleep(timeCmd)
			sm.logger(hal.LogLevelDebug, "Sending EventReadyToScoot after fast initialization")
			sm.SendEvent(EventReadyToScoot)
		}()

		return nil
	}

	// For inactive battery, do the full status read as before
	time.Sleep(timeStateVerify)

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
	sm.reader.dataMutex.Lock()
	sm.reader.readyToScoot = true // Just set it true, don't wait for response
	sm.reader.justInserted = true
	sm.reader.data.Present = true
	batteryState := sm.reader.data.State
	sm.reader.dataMutex.Unlock()

	sm.logger(hal.LogLevelInfo, fmt.Sprintf("Battery initialization complete - state: %s", batteryState))

	// Send the ready event asynchronously to avoid deadlock
	go func() {
		time.Sleep(timeCmd) // Small delay to ensure action completes
		sm.logger(hal.LogLevelDebug, "Sending EventReadyToScoot after initialization")
		sm.SendEvent(EventReadyToScoot)
	}()

	return nil
}

func (sm *BatteryStateMachine) actionBatteryReady(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.dataMutex.Lock()
	sm.reader.readyToScoot = true
	sm.reader.justInserted = false
	// Don't force the state to idle - keep the actual battery state that was read
	currentBatteryState := sm.reader.data.State
	sm.reader.dataMutex.Unlock()

	// Update Redis with initial state
	if err := sm.reader.updateRedisStatus(); err != nil {
		return fmt.Errorf("failed to update Redis: %w", err)
	}

	// For active role battery, always activate regardless of conditions
	if sm.reader.role == BatteryRoleActive {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Battery %d (active role) initialization: triggering activation", sm.reader.index))
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
		// Inactive battery - check seatbox state
		sm.reader.service.Lock()
		seatboxOpen := sm.reader.service.seatboxOpen
		sm.reader.service.Unlock()

		if seatboxOpen {
			// Send event asynchronously
			go func() {
				time.Sleep(timeCmd)
				sm.SendEvent(EventSeatboxOpened) // This will transition to IdleStandby
			}()
		}
	}

	return nil
}

func (sm *BatteryStateMachine) actionBatteryRemoved(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.dataMutex.Lock()
	sm.reader.data.Present = false
	sm.reader.readyToScoot = false
	sm.reader.justInserted = false
	sm.reader.halReinitCount = 0 // Reset HAL reinit counter for clean state
	sm.reader.dataMutex.Unlock()

	// Update Redis immediately
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after battery removal: %v", err))
	}

	// Send BatteryRemoved command asynchronously to prevent blocking
	go func() {
		ctx := sm.reader.getOperationContext()
		if err := sm.reader.sendCommand(ctx, BatteryCommandBatteryRemoved); err != nil {
			sm.logger(hal.LogLevelDebug, fmt.Sprintf("Failed to send BatteryRemoved command (expected for removed battery): %v", err))
		}
	}()

	return nil
}

func (sm *BatteryStateMachine) actionSeatboxOpened(machine *BatteryStateMachine, event BatteryEvent) error {
	// SAFETY: Cancel any ongoing operations when seatbox opens
	sm.reader.dataMutex.Lock()
	if sm.reader.operationCancel != nil {
		sm.reader.operationCancel()
		// Create new context for the command sequence
		sm.reader.operationCtx, sm.reader.operationCancel = context.WithCancel(sm.reader.service.ctx)
	}
	batteryState := sm.reader.data.State
	sm.reader.dataMutex.Unlock()

	ctx := sm.reader.getOperationContext()

	// OEM sequence: 1. OFF -> 2. SEATBOX_OPENED -> 3. INSERTED_IN_SCOOTER

	// Step 1: Send OFF command if battery is active
	if batteryState == BatteryStateActive {
		if err := sm.reader.sendCommand(ctx, BatteryCommandOff); err != nil {
			return fmt.Errorf("failed to send OFF during seatbox open sequence: %w", err)
		}
		time.Sleep(timeCmd) // Wait between commands
	}

	// Step 2: Send UserOpenedSeatbox command
	if err := sm.reader.sendCommand(ctx, BatteryCommandUserOpenedSeatbox); err != nil {
		return fmt.Errorf("failed to send UserOpenedSeatbox: %w", err)
	}
	time.Sleep(timeCmd) // Wait between commands

	// Step 3: Send InsertedInScooter command to reestablish scooter context
	if err := sm.reader.sendCommand(ctx, BatteryCommandInsertedInScooter); err != nil {
		return fmt.Errorf("failed to send InsertedInScooter during seatbox open sequence: %w", err)
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
	// Only active role battery can be activated
	if sm.reader.role != BatteryRoleActive {
		return nil
	}

	// Check seatbox state first - CRITICAL: never activate while seatbox is open
	sm.reader.service.Lock()
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	if seatboxOpen {
		sm.logger(hal.LogLevelWarning, "SAFETY: Cannot activate battery while seatbox is open")
		return fmt.Errorf("activation blocked: seatbox is open")
	}

	// Check pre-conditions
	sm.reader.dataMutex.Lock()
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	state := sm.reader.data.State
	sm.reader.dataMutex.Unlock()

	if !enabled || !ready || lowSOC {
		return fmt.Errorf("activation preconditions not met: enabled=%v, ready=%v, lowSOC=%v, seatboxOpen=%v", enabled, ready, lowSOC, seatboxOpen)
	}

	if state == BatteryStateActive {
		// Send event asynchronously to avoid deadlock
		go func() {
			time.Sleep(timeCmd)
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
			time.Sleep(timeCmd)
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

		sm.reader.dataMutex.Lock()
		newState := sm.reader.data.State
		sm.reader.dataMutex.Unlock()

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

			sm.reader.dataMutex.Lock()
			recoveryState := sm.reader.data.State
			sm.reader.dataMutex.Unlock()

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
	// SAFETY: Cancel any ongoing operations immediately when seatbox opens
	sm.reader.dataMutex.Lock()
	if sm.reader.operationCancel != nil {
		sm.reader.operationCancel()
		// Create new context for the OFF command
		sm.reader.operationCtx, sm.reader.operationCancel = context.WithCancel(sm.reader.service.ctx)
	}
	state := sm.reader.data.State
	sm.reader.dataMutex.Unlock()

	sm.logger(hal.LogLevelWarning, "SAFETY: Seatbox opened - cancelling operations and sending OFF command")

	if state == BatteryStateIdle || state == BatteryStateAsleep {
		// Send event asynchronously to avoid deadlock
		go func() {
			time.Sleep(timeCmd)
			sm.SendEvent(EventStateVerified)
		}()
		return nil
	}

	// Send OFF command immediately (respecting 400ms hardware timing)
	if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandOff); err != nil {
		// Check if context was cancelled (tag departed)
		if strings.Contains(err.Error(), "context cancel") || strings.Contains(err.Error(), "invalid state") {
			sm.logger(hal.LogLevelInfo, "Deactivation command cancelled due to tag departure")
			// Don't send failure event - let tag departure handling take over
			return nil
		}
		// Schedule failure event for other errors
		go func() {
			time.Sleep(timeCmd)
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
			sm.reader.dataMutex.Lock()
			newState := sm.reader.data.State
			sm.reader.dataMutex.Unlock()

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
	// Use debounced fault setting to prevent flapping
	sm.reader.setFaultWithDebounce("NotFollowingCommand", true)

	sm.logger(hal.LogLevelError, "Battery activation failed")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionDeactivationFailed(machine *BatteryStateMachine, event BatteryEvent) error {
	// Use debounced fault setting to prevent flapping
	sm.reader.setFaultWithDebounce("NotFollowingCommand", true)

	sm.logger(hal.LogLevelError, "Battery deactivation failed")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionHeartbeatInError(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, "Heartbeat in error state - attempting recovery")

	// Check HAL reinit counter first
	sm.reader.dataMutex.Lock()
	halReinitCount := sm.reader.halReinitCount
	sm.reader.dataMutex.Unlock()

	// If we've had too many HAL reinits, assume battery is gone
	const maxHALReinits = 5
	if halReinitCount >= maxHALReinits {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("HAL reinit count reached %d, assuming battery departed", halReinitCount))
		// Reset the counter to avoid repeated triggers
		sm.reader.dataMutex.Lock()
		sm.reader.halReinitCount = 0
		sm.reader.dataMutex.Unlock()

		// Send battery removed event to handle departure
		go func() {
			time.Sleep(timeCmd)
			sm.SendEvent(EventBatteryRemoved)
		}()
		return nil
	}

	// Read current battery status to check actual state
	if err := sm.reader.readBatteryStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to read status in error recovery: %v", err))

		// Check if the failure is because the tag has definitively departed.
		if strings.Contains(err.Error(), "tag departed") || strings.Contains(err.Error(), "discovering state") {
			sm.logger(hal.LogLevelInfo, "Recovery failed because tag is gone. Transitioning to NotPresent.")
			// Send the final event to correctly mark the battery as removed.
			go func() {
				time.Sleep(timeCmd)
				sm.SendEvent(EventBatteryRemoved)
			}()
			// Return nil here because we have successfully handled the error by confirming departure.
			return nil
		}

		return err
	}

	// Check if battery is actually in a good state now
	sm.reader.dataMutex.Lock()
	batteryState := sm.reader.data.State
	present := sm.reader.data.Present
	sm.reader.dataMutex.Unlock()

	if present && (batteryState == BatteryStateIdle || batteryState == BatteryStateAsleep) {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Battery recovered to %s state, transitioning out of error", batteryState))
		// Send recovery event
		go func() {
			time.Sleep(timeCmd)
			sm.SendEvent(EventHALRecovered)
		}()
	} else if present && batteryState == BatteryStateActive {
		sm.logger(hal.LogLevelInfo, "Battery is active, need to align state machine")
		// Battery is active but state machine is in error state
		// Send event to transition directly to Active
		go func() {
			time.Sleep(timeCmd)
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
	// Handle different logic for active vs inactive batteries
	if sm.reader.IsInactive() {
		return sm.actionMaintenanceHeartbeat(machine, event)
	}

	// Get current state information
	sm.reader.service.Lock()
	seatboxOpen := sm.reader.service.seatboxOpen
	sm.reader.service.Unlock()

	sm.reader.dataMutex.Lock()
	currentState := sm.reader.data.State
	sm.reader.dataMutex.Unlock()

	// If seatbox is open, just send heartbeat
	if seatboxOpen {
		sm.logger(hal.LogLevelDebug, "Sending ScooterHeartbeat (Seatbox Open)")
		err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandScooterHeartbeat)
		if sm.handleCmdError(err) {
			return nil // Departure detected and handled, do not transition to Error state.
		}
		return err // Propagate other, unexpected errors.
	}

	// Seatbox is closed - send heartbeat and ensure battery stays on
	sm.logger(hal.LogLevelDebug, "Heartbeat tick: Seatbox closed")

	// Send UserClosedSeatbox command
	if err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandUserClosedSeatbox); err != nil {
		if sm.handleCmdError(err) {
			return nil
		}
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send SEATBOX_CLOSED: %v", err))
		// Don't return, as this is not critical. The main ON/Heartbeat is more important.
	}
	time.Sleep(timeCmd)

	// Always ensure battery is ON
	if currentState != BatteryStateActive {
		sm.logger(hal.LogLevelInfo, "Battery not active, sending ON command")
		err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandOn)
		if sm.handleCmdError(err) {
			return nil
		}

		// Wait for command to complete, then read status to verify
		time.Sleep(timeCmd)
		if err := sm.reader.readBatteryStatus(); err != nil {
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to verify status after ON command: %v", err))
		} else {
			// Check if battery became active after ON command
			sm.reader.dataMutex.Lock()
			batteryState := sm.reader.data.State
			sm.reader.dataMutex.Unlock()

			if batteryState == BatteryStateActive {
				sm.logger(hal.LogLevelInfo, "Battery activated successfully")
				go func() {
					time.Sleep(timeCmd)
					sm.SendEvent(EventBatteryAlreadyActive)
				}()
			} else if !seatboxOpen {
				sm.logger(hal.LogLevelInfo, "Battery not active after ON command, triggering activation sequence")
				go func() {
					time.Sleep(timeCmd)
					sm.SendEvent(EventSeatboxClosed)
				}()
			}
		}

		return err
	} else {
		// Battery is already active, send heartbeat and verify
		sm.logger(hal.LogLevelDebug, "Battery already active, sending heartbeat")
		err := sm.reader.sendCommand(sm.reader.getOperationContext(), BatteryCommandScooterHeartbeat)
		if sm.handleCmdError(err) {
			return nil
		}
		if err != nil {
			// Log the error but don't propagate it, as it's a non-critical heartbeat failure.
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to send heartbeat: %v", err))
		} else {
			// Wait for command to complete, then read status to verify and update metrics
			time.Sleep(timeCmd)
			if err := sm.reader.readBatteryStatus(); err != nil {
				sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to verify status after heartbeat: %v", err))
			} else {
				// Check if battery state changed unexpectedly
				sm.reader.dataMutex.Lock()
				batteryState := sm.reader.data.State
				charge := sm.reader.data.Charge
				voltage := sm.reader.data.Voltage
				sm.reader.dataMutex.Unlock()

				if batteryState != BatteryStateActive {
					sm.logger(hal.LogLevelWarning, fmt.Sprintf("Battery state changed unexpectedly to %s during heartbeat", batteryState))
				} else {
					sm.logger(hal.LogLevelDebug, fmt.Sprintf("Heartbeat verified: SOC=%d%%, Voltage=%dmV", charge, voltage))
				}
			}
		}
	}

	return nil
}

func (sm *BatteryStateMachine) actionMaintenanceHeartbeat(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelDebug, fmt.Sprintf("Maintenance heartbeat for inactive battery %d", sm.reader.index))

	// For inactive batteries, we only need to read status to detect changes
	// No commands are sent to avoid interfering with battery operation
	prevPresent := false
	sm.reader.dataMutex.RLock()
	prevPresent = sm.reader.data.Present
	prevState := sm.reader.data.State
	sm.reader.dataMutex.RUnlock()

	// Attempt to read battery status
	err := sm.reader.readBatteryStatus()
	if err != nil {
		// Check if this is a tag departure error
		if sm.handleCmdError(err) {
			// Tag departure was handled, check if we need to update state
			if prevPresent {
				sm.logger(hal.LogLevelInfo, fmt.Sprintf("Inactive battery %d departed during maintenance poll", sm.reader.index))
			}
			return nil
		}

		// For other errors, if battery was previously present, it might have departed
		if prevPresent {
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to read inactive battery %d status, treating as departure: %v", sm.reader.index, err))
			sm.reader.handleTagAbsent()
		}

		return err
	}

	// Read was successful - check for state changes
	sm.reader.dataMutex.RLock()
	currentPresent := sm.reader.data.Present
	currentState := sm.reader.data.State
	charge := sm.reader.data.Charge
	voltage := sm.reader.data.Voltage
	sm.reader.dataMutex.RUnlock()

	// Detect new insertions
	if !prevPresent && currentPresent {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Inactive battery %d insertion detected during maintenance poll", sm.reader.index))
		// The readBatteryStatus already updated the data and Redis, so we just need to log
	}

	// Detect state changes
	if prevPresent && currentPresent && prevState != currentState {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Inactive battery %d state changed: %s -> %s", sm.reader.index, prevState, currentState))
	}

	// Log periodic status for monitoring
	if currentPresent {
		sm.logger(hal.LogLevelDebug, fmt.Sprintf("Inactive battery %d maintenance poll: SOC=%d%%, Voltage=%dmV, State=%s",
			sm.reader.index, charge, voltage, currentState))
	}

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

	sm.reader.dataMutex.Lock()
	ready := sm.reader.readyToScoot
	lowSOC := sm.reader.data.LowSOC
	enabled := sm.reader.enabled
	sm.reader.dataMutex.Unlock()

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
	sm.reader.dataMutex.Lock()
	sm.reader.data.Faults.CommunicationError = true
	sm.reader.dataMutex.Unlock()

	sm.logger(hal.LogLevelError, "Battery command failed")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionHALError(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.dataMutex.Lock()
	sm.reader.data.Faults.ReaderError = true
	sm.reader.dataMutex.Unlock()

	sm.logger(hal.LogLevelError, "HAL error occurred - triggering full recovery sequence")

	// Update Redis first
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis during HAL error: %v", err))
	}

	sm.logger(hal.LogLevelInfo, "Deinitializing HAL for recovery...")

	// Deinitialize the HAL
	sm.reader.hal.Deinitialize()

	sm.logger(hal.LogLevelInfo, fmt.Sprintf("Waiting %v before reinitializing...", timeReinit))
	time.Sleep(timeReinit)

	// Reinitialize the HAL
	sm.logger(hal.LogLevelInfo, "Reinitializing HAL after recovery wait...")
	if err := sm.reader.hal.Initialize(); err != nil {
		sm.logger(hal.LogLevelError, fmt.Sprintf("Failed to reinitialize HAL during recovery: %v", err))
		// Schedule another recovery attempt via heartbeat
		return err
	}

	sm.logger(hal.LogLevelInfo, "HAL recovery sequence completed successfully")

	// Trigger a recovery event to transition out of error state
	go func() {
		time.Sleep(100 * time.Millisecond) // Small delay to let state settle
		sm.SendEvent(EventHALRecovered)
	}()

	return nil
}

func (sm *BatteryStateMachine) actionRecovery(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.dataMutex.Lock()
	sm.reader.data.Faults.ReaderError = false
	sm.reader.data.Faults.CommunicationError = false
	sm.reader.dataMutex.Unlock()

	// Use debounced fault clearing to prevent flapping
	sm.reader.setFaultWithDebounce("NotFollowingCommand", false)

	sm.logger(hal.LogLevelInfo, "Recovery from error state")
	return sm.reader.updateRedisStatus()
}

func (sm *BatteryStateMachine) actionDisable(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.dataMutex.Lock()
	sm.reader.enabled = false
	sm.reader.dataMutex.Unlock()

	sm.logger(hal.LogLevelInfo, "Battery reader disabled")
	return nil
}

func (sm *BatteryStateMachine) actionEnable(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.reader.dataMutex.Lock()
	sm.reader.enabled = true
	sm.reader.dataMutex.Unlock()

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

	sm.reader.dataMutex.Lock()
	batteryPresent := sm.reader.data.Present
	sm.reader.dataMutex.Unlock()

	// Always enable battery 0 if it's present
	if batteryPresent {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 (present=%v, seatboxOpen=%v)", batteryPresent, seatboxOpen))

		// Enable the battery
		sm.reader.dataMutex.Lock()
		sm.reader.enabled = true
		sm.reader.dataMutex.Unlock()

		// Send EventEnabled to trigger proper initialization and activation sequence
		go func() {
			// Small delay to ensure state transition completes first
			time.Sleep(timeCmd)
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

	sm.reader.dataMutex.Lock()
	batteryPresent := sm.reader.data.Present
	sm.reader.dataMutex.Unlock()

	// Always enable battery 0 if it's present
	if batteryPresent {
		sm.logger(hal.LogLevelInfo, fmt.Sprintf("Enabling Battery 0 (present=%v, seatboxOpen=%v)", batteryPresent, seatboxOpen))

		// Enable the battery
		sm.reader.dataMutex.Lock()
		sm.reader.enabled = true
		sm.reader.dataMutex.Unlock()

		// Schedule a tag arrival event to trigger initialization
		go func() {
			// Small delay to ensure state transition completes first
			time.Sleep(timeCmd)
			sm.logger(hal.LogLevelDebug, "Sending EventTagArrived after enabling")
			sm.SendEvent(EventTagArrived)
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
	sm.logger(hal.LogLevelInfo, "Tag may have departed. Starting discovery confirmation timeout...")

	// Cancel any pending operations immediately since tag might be gone
	sm.reader.dataMutex.Lock()
	if sm.reader.operationCancel != nil {
		sm.logger(hal.LogLevelDebug, "Cancelling pending operations due to potential tag departure")
		sm.reader.operationCancel()
	}
	// Create new operation context for future operations
	sm.reader.operationCtx, sm.reader.operationCancel = context.WithCancel(sm.reader.service.ctx)
	sm.reader.dataMutex.Unlock()

	// Start a single timer. If a tag arrives, the FSM will transition away from
	// StateDiscovering. If not, this timeout will fire.
	go func() {
		time.Sleep(timeDeparture) // 500ms confirmation timeout matching C's BMS_TIME_DEPARTURE
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
	sm.logger(hal.LogLevelInfo, "Discovery timed out. Confirming battery is not present.")

	sm.reader.dataMutex.Lock()
	// Only update if the state was previously present
	if sm.reader.data.Present {
		sm.reader.data.Present = false
		sm.reader.justInserted = false
		sm.reader.readyToScoot = false

		// Update Redis with the definitive "not present" state
		if err := sm.reader.updateRedisStatus(); err != nil {
			sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after confirmed departure: %v", err))
		}
	}
	sm.reader.dataMutex.Unlock()

	return nil
}

func (sm *BatteryStateMachine) actionStateTimeout(machine *BatteryStateMachine, event BatteryEvent) error {
	sm.logger(hal.LogLevelWarning, "State timeout reached - assuming battery has been removed")

	// Mark battery as not present to force cleanup
	sm.reader.dataMutex.Lock()
	sm.reader.data.Present = false
	sm.reader.justInserted = false
	sm.reader.readyToScoot = false

	// Update Redis to reflect battery removal
	if err := sm.reader.updateRedisStatus(); err != nil {
		sm.logger(hal.LogLevelWarning, fmt.Sprintf("Failed to update Redis after state timeout: %v", err))
	}
	sm.reader.dataMutex.Unlock()

	// Start discovery to detect if battery is actually still present
	return nil
}
