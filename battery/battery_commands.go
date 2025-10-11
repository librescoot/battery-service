package battery

import (
	"time"
)

func (r *BatteryReader) updateLastCmdTime() {
	r.lastCmdTime = time.Now()
}

func (r *BatteryReader) getRemainingCmdTime() time.Duration {
	elapsed := time.Since(r.lastCmdTime)
	required := BMSTimeCmd
	if elapsed >= required {
		return 0
	}
	return required - elapsed
}

func (r *BatteryReader) getOpenedTime() time.Duration {
	if r.justInserted {
		return BMSTimeCmd
	}
	if r.justOpened {
		if r.data.State == BMSStateAsleep {
			return BMSTimeCmdFirstOpenedAsleep
		} else {
			return BMSTimeCmdFirstOpenedAwake
		}
	}
	return BMSTimeCmdSlow
}

func (r *BatteryReader) getInsertedTime() time.Duration {
	if r.justInserted {
		return BMSTimeCmd
	}
	return BMSTimeCmdSlow
}

func (r *BatteryReader) sendOnOff() {
	var cmd BMSCommand
	if r.enabled && !r.batteryEmpty() {
		cmd = BMSCmdOn
	} else {
		cmd = BMSCmdOff
	}
	r.writeCommand(cmd)
}

func (r *BatteryReader) batteryEmpty() bool {
	return r.data.LowSOC && r.data.Charge <= BMSMinSOC
}

func (r *BatteryReader) checkInactive() bool {
	correct := (r.data.State == BMSStateAsleep) || (r.data.State == BMSStateIdle)
	r.setFault(BMSFaultBMSNotFollowingCmd, !correct)
	return correct
}

func (r *BatteryReader) checkStateCorrect(raiseFault bool) bool {
	var correct bool
	if r.enabled && !r.batteryEmpty() {
		correct = (r.data.State == BMSStateActive)
	} else {
		correct = (r.data.State == BMSStateAsleep) || (r.data.State == BMSStateIdle)
	}

	if raiseFault {
		tryingToSwitchOnWithLowSOCOrFault := r.enabled &&
			(r.data.LowSOC || r.data.FaultCode != 0)

		faultCriteria := !correct &&
			!tryingToSwitchOnWithLowSOCOrFault &&
			r.data.EmptyOr0Data == 0

		r.setFault(BMSFaultBMSNotFollowingCmd, faultCriteria)
	}

	return correct
}

func (r *BatteryReader) handleDeparture() {
	r.service.logger.Infof("Battery %d: Tag departed", r.index)
	r.data = BMSData{}
	r.sendStatusUpdate()
}

func (r *BatteryReader) retryZeroDataOrEmptyBattery() {
	if r.data.EmptyOr0Data <= BMSMaxZeroRetryHeartbeat {
		// Continue heartbeat to retry
		r.setupHeartbeatTimer()
	} else {
		// Exceeded threshold - stop heartbeat, will rely on passive discovery
		r.service.logger.Warnf("Battery %d: Giving up active recovery after %d attempts (~110s)", r.index, r.data.EmptyOr0Data)
		r.clearHeartbeatTimer()
	}
}

func (r *BatteryReader) stopTimerIfBatteryEmpty() {
	if r.data.LowSOC && r.data.Charge <= BMSMinSOC {
		r.clearHeartbeatTimer()
		r.data.State = BMSStateIdle
		r.sendStatusUpdate()
	}
}

func (r *BatteryReader) setupHeartbeatTimer() {
	var heartbeatInterval time.Duration

	if r.vehicleState == VehicleStateStandby {
		if r.enabled {
			heartbeatInterval = HeartbeatIntervalActiveStandby
		} else {
			heartbeatInterval = HeartbeatIntervalInactive
		}
	} else {
		if r.enabled {
			heartbeatInterval = BMSTimeUpdateOn
		} else {
			heartbeatInterval = HeartbeatIntervalInactive
		}
	}

	r.service.logger.Debugf("Battery %d: Setting up heartbeat timer for %s (vehicleState=%s, enabled=%t, state=%s)",
		r.index, heartbeatInterval, r.vehicleState, r.enabled, r.state)

	if r.heartbeatTimer == nil {
		// First time setup
		r.heartbeatTimer = time.NewTimer(heartbeatInterval)
	} else {
		// Reset existing timer - this preserves the channel
		if !r.heartbeatTimer.Stop() {
			// Timer already fired, drain the channel
			select {
			case <-r.heartbeatTimer.C:
			default:
			}
		}
		r.heartbeatTimer.Reset(heartbeatInterval)
	}

	if !r.heartbeatRunning {
		r.heartbeatRunning = true
		r.service.logger.Debugf("Battery %d: Starting heartbeat monitor goroutine", r.index)
		go r.heartbeatMonitor()
	}
}

func (r *BatteryReader) clearHeartbeatTimer() {
	if r.heartbeatTimer != nil {
		// Stop the timer but don't set to nil - preserve the channel for heartbeat monitor
		r.heartbeatTimer.Stop()
	}
}

func (r *BatteryReader) getHeartbeatInterval() time.Duration {
	if r.vehicleState == VehicleStateStandby {
		if r.enabled {
			return r.service.config.HeartbeatTimeout
		} else {
			return r.service.config.OffUpdateTime
		}
	} else {
		if r.enabled {
			return BMSTimeUpdateOn
		} else {
			return r.service.config.OffUpdateTime
		}
	}
}

func (r *BatteryReader) heartbeatMonitor() {
	defer func() {
		r.heartbeatRunning = false
	}()

	for {
		select {
		case <-r.stopChan:
			return

		case <-r.getHeartbeatTimer():
			// Heartbeat timeout during any heartbeat-related state
			r.service.logger.Debugf("Battery %d: Heartbeat timer fired, state=%s, inHeartbeatTree=%t",
				r.index, r.state, r.isInHeartbeatTree())
			if r.isInHeartbeatTree() {
				stateCorrect := r.checkStateCorrect(false)
				r.service.logger.Debugf("Battery %d: State correct check: %t (current=%s, expected=%s)",
					r.index, stateCorrect, r.data.State, func() string {
						if r.enabled && !r.batteryEmpty() {
							return "active"
						}
						return "asleep/idle"
					}())
				if !stateCorrect {
					// Battery state wrong after timeout - force rediscovery
					r.service.logger.Warnf("Battery %d: Recovery timeout after %s - forcing rediscovery", r.index, r.getHeartbeatInterval())
					r.handleDeparture()
					r.transitionTo(StateDiscoverTag)
				} else {
					// State correct - restart heartbeat cycle
					r.service.logger.Warnf("Battery %d: Heartbeat timeout - restarting cycle", r.index)
					r.transitionTo(StateHeartbeat)
				}
			}
		}
	}
}

func (r *BatteryReader) getHeartbeatTimer() <-chan time.Time {
	if r.heartbeatTimer == nil {
		return nil
	}
	return r.heartbeatTimer.C
}

// Check if we're in any heartbeat-related state (including recovery states)
func (r *BatteryReader) isInHeartbeatTree() bool {
	switch r.state {
	case StateHeartbeat, StateHeartbeatActions, StateSendClosed,
		StateSendOnOff, StateCondStateOK, StateSendInsertedClosed, StateWaitUpdate:
		return true
	default:
		return false
	}
}

// takeInhibitor acquires a systemd suspend inhibitor lock to prevent system suspend
// during critical NFC operations.
//
// The function retries up to BMSMaxRetryTakeInhibitor times with a 1ms delay between
// attempts. If acquisition fails, the error is logged but the system continues to
// operate (degraded mode).
func (r *BatteryReader) takeInhibitor() {
	// If we already have an inhibitor, don't acquire another one
	if r.suspendInhibitor != nil && r.suspendInhibitor.IsActive() {
		r.service.logger.Debugf("Battery %d: Inhibitor already acquired", r.index)
		return
	}

	var lastErr error
	for attempt := 0; attempt < BMSMaxRetryTakeInhibitor; attempt++ {
		inhibitor, err := NewSuspendInhibitor(
			"BATTERY_NFC_TRANSACTION_INHIBITOR",
			"NFC_TRANSACTION",
			"block",
		)
		if err == nil {
			r.suspendInhibitor = inhibitor
			r.service.logger.Debugf("Battery %d: Inhibitor acquired", r.index)
			return
		}

		lastErr = err
		if attempt == 0 {
			r.service.logger.Debugf("Battery %d: Failed to acquire suspend inhibitor (attempt %d/%d): %v",
				r.index, attempt+1, BMSMaxRetryTakeInhibitor, err)
		} else {
			r.service.logger.Warnf("Battery %d: Failed to acquire suspend inhibitor (attempt %d/%d): %v",
				r.index, attempt+1, BMSMaxRetryTakeInhibitor, err)
		}

		if attempt < BMSMaxRetryTakeInhibitor-1 {
			time.Sleep(1 * time.Millisecond)
		}
	}

	// If we exhausted all retries, log the final error
	r.service.logger.Errorf("Battery %d: Failed to acquire suspend inhibitor after %d attempts: %v",
		r.index, BMSMaxRetryTakeInhibitor, lastErr)
}

// releaseInhibitor releases the systemd suspend inhibitor lock, allowing the system
// to suspend again. This should be called after critical NFC operations complete.
func (r *BatteryReader) releaseInhibitor() {
	if r.suspendInhibitor == nil {
		return
	}

	r.service.logger.Debugf("Battery %d: Releasing inhibitor", r.index)

	if err := r.suspendInhibitor.Release(); err != nil {
		r.service.logger.Warnf("Battery %d: Failed to release inhibitor: %v", r.index, err)
	}

	r.suspendInhibitor = nil
}

