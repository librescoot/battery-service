package battery

import (
	"fmt"
	"time"

	"battery-service/nfc/hal"
)

// startHeartbeat starts the heartbeat goroutine that sends periodic events to the state machine
func (r *BatteryReader) startHeartbeat() {
	go func() {
		ticker := time.NewTicker(timeHeartbeatIntervalScooter)
		defer ticker.Stop()
		
		var lastActiveStatusPoll time.Time
		var lastMaintenancePoll time.Time

		for {
			select {
			case <-r.stopChan:
				return
			case <-ticker.C:
				r.Lock()
				present := r.data.Present
				state := r.data.State
				r.Unlock()

				if !present {
					continue
				}

				// Send heartbeat event to state machine
				r.stateMachine.SendEvent(EventHeartbeatTick)
				
				// Monitor queue depth every heartbeat
				queueDepth := r.stateMachine.GetEventQueueDepth()
				if queueDepth > 20 {
					r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Event queue depth high: %d", queueDepth))
				}

				// Check if we need to do periodic status polling
				if r.IsActive() && state == BatteryStateActive && 
					time.Since(lastActiveStatusPoll) >= timeActiveStatusPoll {
					r.logCallback(hal.LogLevelDebug, "Time for active status poll")
					r.stateMachine.SendEvent(EventMaintenanceTick)
					lastActiveStatusPoll = time.Now()
				}


				// Inactive battery maintenance polling
				if r.IsInactive() && time.Since(lastMaintenancePoll) >= timeBattery1MaintPollInterval {
					r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Inactive battery %d: Time for maintenance poll", r.index))
					
					// Just read status
					if err := r.readBatteryStatus(); err != nil {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Inactive battery %d: Error during maintenance poll: %v", r.index, err))
					}
					
					lastMaintenancePoll = time.Now()
				}

				// General active battery polling every timeActiveStatusPoll seconds
				if r.IsActive() && time.Since(lastActiveStatusPoll) >= timeActiveStatusPoll {
					// Skip if already handled above
					if state != BatteryStateActive {
						// For non-active states, send maintenance tick to state machine
						// The state machine will decide what to do based on current state
						r.logCallback(hal.LogLevelDebug, "Time for maintenance poll (non-active state)")
						r.stateMachine.SendEvent(EventMaintenanceTick)
					}
					lastActiveStatusPoll = time.Now()
				}
			}
		}
	}()
}