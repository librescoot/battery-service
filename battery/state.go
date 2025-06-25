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
	index := r.index
	r.Unlock()

	// Get additional state info for detailed logging
	r.RLock()
	currentState := r.data.State
	enabled := r.enabled
	ready := r.readyToScoot
	r.RUnlock()
	
	machineState := r.stateMachine.GetCurrentState()
	
	r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery %d: Seatbox state changed to %s (present=%v, batteryState=%s, machineState=%s, enabled=%v, ready=%v)", 
		index, map[bool]string{true: "OPEN", false: "CLOSED"}[isOpen], present, currentState, machineState, enabled, ready))

	if !present {
		r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery %d: Ignoring seatbox event - battery not present", index))
		return // Ignore if battery not present
	}

	// Send the appropriate event to the state machine
	if isOpen {
		r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery %d: Sending EventSeatboxOpened to state machine (current machine state: %s)", index, machineState))
		r.stateMachine.SendEvent(EventSeatboxOpened)
	} else {
		r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery %d: Sending EventSeatboxClosed to state machine (current machine state: %s)", index, machineState))
		r.stateMachine.SendEvent(EventSeatboxClosed)
	}
}
