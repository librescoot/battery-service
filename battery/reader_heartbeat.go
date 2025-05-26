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
		var lastLowFreqPoll time.Time

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

				// Check if we need to do periodic status polling
				if r.index == 0 && state == BatteryStateActive && 
					time.Since(lastActiveStatusPoll) >= timeActiveStatusPoll {
					r.logCallback(hal.LogLevelDebug, "Time for active status poll")
					r.stateMachine.SendEvent(EventMaintenanceTick)
					lastActiveStatusPoll = time.Now()
				}

				// Check if we need low-frequency polling for idle/asleep batteries
				if r.index == 0 && (state == BatteryStateIdle || state == BatteryStateAsleep) {
					r.service.Lock()
					vehicleState := r.service.vehicleState
					cbCharge := r.service.cbBatteryCharge
					r.service.Unlock()

					if vehicleState == "stand-by" && cbCharge >= cbBatteryActivationThreshold &&
						time.Since(lastLowFreqPoll) >= timeMaintPollIdleAsleepInterval {
						r.logCallback(hal.LogLevelDebug, "Time for low-frequency idle/asleep poll")
						
						// Power up HAL if needed
						r.Lock()
						if r.isPoweredDown {
							r.logCallback(hal.LogLevelInfo, "Powering up HAL for low-frequency poll")
							r.isPoweredDown = false
							r.Unlock()
							
							if err := r.simpleHALRecovery(); err != nil {
								r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed HAL recovery: %v", err))
								lastLowFreqPoll = time.Now()
								continue
							}
						} else {
							r.Unlock()
						}

						// Poll status
						if err := r.readBatteryStatus(); err != nil {
							r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Error during low-freq poll: %v", err))
						}

						// Power down HAL again
						r.Lock()
						if !r.isPoweredDown && (r.data.State == BatteryStateIdle || r.data.State == BatteryStateAsleep) {
							r.logCallback(hal.LogLevelInfo, "Powering down HAL after low-frequency poll")
							r.Unlock()
							
							if err := r.safelyPowerDownHAL(); err != nil {
								r.logCallback(hal.LogLevelError, fmt.Sprintf("Failed to power down HAL: %v", err))
							}
						} else {
							r.Unlock()
						}
						
						lastLowFreqPoll = time.Now()
					}
				}

				// Battery 1 maintenance polling
				if r.index == 1 && time.Since(lastMaintenancePoll) >= timeBattery1MaintPollInterval {
					r.logCallback(hal.LogLevelDebug, "Battery 1: Time for maintenance poll")
					
					// Just read status
					if err := r.readBatteryStatus(); err != nil {
						r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Battery 1: Error during maintenance poll: %v", err))
					}
					
					lastMaintenancePoll = time.Now()
				}

				// General battery 0 polling every 10 seconds
				if r.index == 0 && time.Since(lastActiveStatusPoll) >= timeActiveStatusPoll {
					// Skip if already handled above
					if state != BatteryStateActive {
						r.service.Lock()
						vehicleState := r.service.vehicleState
						cbCharge := r.service.cbBatteryCharge
						r.service.Unlock()

						// Skip if covered by low-frequency polling
						skipGeneralPoll := vehicleState == "stand-by" && 
							cbCharge >= cbBatteryActivationThreshold &&
							(state == BatteryStateIdle || state == BatteryStateAsleep) &&
							time.Since(lastLowFreqPoll) < timeMaintPollIdleAsleepInterval

						if !skipGeneralPoll {
							r.logCallback(hal.LogLevelDebug, "Battery 0: General status poll")
							if err := r.readBatteryStatus(); err != nil {
								r.logCallback(hal.LogLevelWarning, fmt.Sprintf("Error during general poll: %v", err))
							}
						}
					}
					lastActiveStatusPoll = time.Now()
				}
			}
		}
	}()
}