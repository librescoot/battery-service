package battery

import (
	"fmt"
	"reflect"

	"battery-service/nfc/hal"

	"github.com/redis/go-redis/v9"
)

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
		"state":              r.data.State.String(),
		"voltage":            fmt.Sprintf("%d", r.data.Voltage),
		"current":            fmt.Sprintf("%d", r.data.Current),
		"charge":             fmt.Sprintf("%d", r.data.Charge),
		"temperature:0":      fmt.Sprintf("%d", r.data.Temperature[0]),
		"temperature:1":      fmt.Sprintf("%d", r.data.Temperature[1]),
		"temperature:2":      fmt.Sprintf("%d", r.data.Temperature[2]),
		"temperature:3":      fmt.Sprintf("%d", r.data.Temperature[3]),
		"temperature-state":  r.data.TemperatureState.String(),
		"cycle-count":        fmt.Sprintf("%d", r.data.CycleCount),
		"state-of-health":    fmt.Sprintf("%d", r.data.StateOfHealth),
		"serial-number":      string(r.data.SerialNumber[:]),
		"manufacturing-date": r.data.ManufacturingDate,
		"fw-version":         r.data.FWVersion,
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

	// Fetch initial vehicle state and cb-battery charge
	s.logger.Printf("[Redis] Fetching initial vehicle state and cb-battery charge")
	s.updateVehicleState()

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
			s.updateSeatboxState()

			// If the vehicle state itself changed, update it
			if msg.Payload == "state" {
				s.logger.Printf("[Redis] Vehicle state change notification received.")
				s.updateVehicleState()
			}
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

// updateVehicleState fetches the current vehicle state from Redis,
// updates the service's internal state.
func (s *Service) updateVehicleState() {
	s.Lock()
	currentVehicleState := s.vehicleState
	s.Unlock()

	newState, err := s.redis.HGet(s.ctx, "vehicle", "state").Result()
	if err != nil {
		if err == redis.Nil {
			s.logger.Printf("[StateUpdate] Vehicle state not found in Redis, assuming empty/unknown.")
			newState = "" // Or a more specific default if applicable
		} else {
			s.logger.Printf("[StateUpdate] Error getting vehicle state from Redis: %v", err)
			return // Don't proceed if we can't get the state
		}
	}

	if newState == currentVehicleState {
		s.logger.Printf("[StateUpdate] Vehicle state unchanged: '%s'.", newState)
		return
	}

	s.logger.Printf("[StateUpdate] Vehicle state changed from '%s' to '%s'", currentVehicleState, newState)

	s.Lock()
	s.vehicleState = newState
	// Collect readers to notify outside the service lock
	readersToNotify := make([]*BatteryReader, 0, 2)
	for _, r := range s.readers {
		if r != nil {
			readersToNotify = append(readersToNotify, r)
		}
	}
	s.Unlock()

	// Send EventVehicleActive to battery readers with active role
	for _, r := range readersToNotify {
		r.Lock()
		present := r.data.Present
		role := r.role
		r.Unlock()
		if present && role == BatteryRoleActive {
			s.logger.Printf("[StateUpdate] Sending EventVehicleActive to Battery %d (active role) for vehicle state '%s'", r.index, newState)
			r.stateMachine.SendEvent(EventVehicleActive)
		}
	}
}

