package battery

import (
	"fmt"
	"time"

	"battery-service/nfc/hal"
)

// startHeartbeat starts the unified smart polling goroutine
func (r *BatteryReader) startHeartbeat() {
	go func() {
		ticker := time.NewTicker(timeHeartbeatIntervalScooter)
		defer ticker.Stop()
		
		var lastStatusPoll time.Time = time.Now() // Initialize to prevent huge time calculations
		var lastSuccessfulOperation time.Time = time.Now()
		var consecutiveFailures int

		for {
			select {
			case <-r.stopChan:
				return
			case <-ticker.C:
				// Smart polling - adaptive based on battery state and role
				r.RLock()
				present := r.data.Present
				state := r.data.State
				isActive := r.role == BatteryRoleActive
				enabled := r.enabled
				r.RUnlock()

				if !present || !enabled {
					// Debug logging to understand why Battery 0 might not be polling
					if r.index == 0 {
						r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0: Skipping poll - present=%v, enabled=%v", present, enabled))
					}
					continue
				}

				// Monitor queue depth every cycle
				queueDepth := r.stateMachine.GetEventQueueDepth()
				if queueDepth > 20 {
					r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Event queue depth high: %d", queueDepth))
				}

				// Check for stuck reader
				if time.Since(lastSuccessfulOperation) > 30*time.Second {
					consecutiveFailures++
					if consecutiveFailures >= 3 {
						r.logCallback(hal.LogLevelError, fmt.Sprintf("Reader appears stuck - no successful operations for %v, triggering full recovery", time.Since(lastSuccessfulOperation)))
						r.Lock()
						r.halReinitCount++
						reinitCount := r.halReinitCount
						r.Unlock()
						r.logCallback(hal.LogLevelInfo, fmt.Sprintf("HAL reinit count: %d", reinitCount))
						
						r.Lock()
						if err := r.hal.FullReinitialize(); err != nil {
							r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to recover stuck reader: %v", err))
							r.Unlock()
							r.stateMachine.SendEvent(EventHALError)
							consecutiveFailures = 0
							lastSuccessfulOperation = time.Now()
							continue
						}
						r.Unlock()
						consecutiveFailures = 0
						lastSuccessfulOperation = time.Now()
					}
				}

				// Adaptive polling based on state and role
				shouldPoll := false
				pollReason := ""
				
				timeSinceLastPoll := time.Since(lastStatusPoll)
				
				if isActive {
					// Active battery (slot 0) - more frequent polling
					if state == BatteryStateActive && timeSinceLastPoll >= timeActiveStatusPoll {
						shouldPoll = true
						pollReason = "active battery status poll"
					} else if state != BatteryStateActive && timeSinceLastPoll >= timeHeartbeatIntervalScooter*2 {
						shouldPoll = true
						pollReason = "active battery state monitoring"
					}
					
					// Debug logging for Battery 0 to understand why polling isn't happening
					if r.index == 0 && !shouldPoll {
						r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0: No poll needed - state=%s, timeSinceLastPoll=%v, activeThreshold=%v, monitoringThreshold=%v", 
							state, timeSinceLastPoll, timeActiveStatusPoll, timeHeartbeatIntervalScooter*2))
					}
				} else {
					// Inactive battery (slot 1) - less frequent polling
					if timeSinceLastPoll >= timeBattery1MaintPollInterval {
						shouldPoll = true
						pollReason = "inactive battery maintenance poll"
					}
				}

				// Presence verification for all batteries - verify presence periodically
				if present && time.Since(lastStatusPoll) >= timeHeartbeatIntervalScooter*6 {
					shouldPoll = true
					pollReason = "presence verification poll"
				}

				// Additional debug for Battery 0 to understand polling behavior
				if r.index == 0 {
					r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery 0: Poll decision - shouldPoll=%v, reason='%s', isActive=%v, state=%s, timeSinceLastPoll=%v", 
						shouldPoll, pollReason, isActive, state, timeSinceLastPoll))
				}

				if shouldPoll {
					r.logCallback(hal.LogLevelDebug, fmt.Sprintf("Battery %d: %s", r.index, pollReason))
					
					// Consolidated status read for all polling needs
					if err := r.readBatteryStatus(); err != nil {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery %d: Error during %s: %v", r.index, pollReason, err))
						
						// If this was a presence verification and failed, battery might be gone
						if pollReason == "presence verification poll" {
							r.logCallback(hal.LogLevelInfo, fmt.Sprintf("Battery %d: Presence verification failed - battery may have been removed", r.index))
							// Trigger explicit tag departure handling
							r.handleTagAbsent()
							r.stateMachine.SendEvent(EventTagDeparted)
						}
					} else {
						lastSuccessfulOperation = time.Now()
						consecutiveFailures = 0
						
						// For active battery, ensure heartbeat commands are sent
						if isActive && r.index == 0 {
							r.stateMachine.SendEvent(EventHeartbeatTick)
						}
					}
					lastStatusPoll = time.Now()
				} else if isActive && r.index == 0 {
					// Send heartbeat event even without polling for active battery
					r.stateMachine.SendEvent(EventHeartbeatTick)
				}
			}
		}
	}()
}