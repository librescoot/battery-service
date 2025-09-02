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
	r.dataMutex.Lock()
	present := r.data.Present
	r.dataMutex.Unlock()

	// For inactive batteries, refresh status when seatbox closes
	// This helps detect battery insertions that might have happened while seatbox was open
	if r.IsInactive() && !isOpen {
		r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Seatbox closed, triggering status refresh for inactive battery %d", r.index))

		// Try to read battery status to detect changes
		if err := r.readBatteryStatus(); err != nil {
			if present {
				// If battery was present but read failed, it might have been removed
				r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Inactive battery %d may have been removed during seatbox close", r.index))
				r.handleTagAbsent()
			}
			// For other cases, we just log the attempt - no need to spam errors
			r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Status refresh failed for inactive battery %d: %v", r.index, err))
		} else {
			// Status read was successful - check if battery was newly detected
			r.dataMutex.RLock()
			nowPresent := r.data.Present
			r.dataMutex.RUnlock()

			if !present && nowPresent {
				r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Inactive battery %d detected during seatbox close", r.index))
			}
		}
	}

	// For inactive batteries, send seatbox commands even if battery not detected as present
	// This ensures the battery gets the seatbox notification if it was recently inserted
	if r.IsInactive() {
		if isOpen {
			r.logCallback(hal.LogLevelInfo, "Seatbox opened - sending command to inactive battery")
			// Try to send the command directly for inactive batteries
			if err := r.sendCommand(r.getOperationContext(), BatteryCommandUserOpenedSeatbox); err != nil {
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Failed to send UserOpenedSeatbox to inactive battery %d: %v", r.index, err))
				// This is expected if no battery is present, so we don't treat it as an error
			}
		} else {
			r.logCallback(hal.LogLevelInfo, "Seatbox closed - sending command to inactive battery")
			// Try to send the command directly for inactive batteries
			if err := r.sendCommand(r.getOperationContext(), BatteryCommandUserClosedSeatbox); err != nil {
				r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Failed to send UserClosedSeatbox to inactive battery %d: %v", r.index, err))
				// This is expected if no battery is present, so we don't treat it as an error
			}
		}
	}

	if !present {
		return // Ignore state machine events if battery not present
	}

	// Send the appropriate event to the state machine for present batteries
	if isOpen {
		r.logCallback(hal.LogLevelInfo, "Seatbox opened")
		r.stateMachine.SendEvent(EventSeatboxOpened)
	} else {
		r.logCallback(hal.LogLevelInfo, "Seatbox closed")
		r.stateMachine.SendEvent(EventSeatboxClosed)
	}
}
