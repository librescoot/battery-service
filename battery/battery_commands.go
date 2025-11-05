package battery

import (
	"fmt"
	"time"
)

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

func (r *BatteryReader) takeInhibitor() {
	if r.suspendInhibitor != nil && r.suspendInhibitor.IsActive() {
		r.logger.Debug(fmt.Sprintf("Inhibitor already acquired"))
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
			r.logger.Debug(fmt.Sprintf("Inhibitor acquired"))
			return
		}

		lastErr = err
		if attempt == 0 {
			r.logger.Debug(fmt.Sprintf("Failed to acquire suspend inhibitor (attempt %d/%d): %v",
				attempt+1, BMSMaxRetryTakeInhibitor, err))
		} else {
			r.logger.Warn(fmt.Sprintf("Failed to acquire suspend inhibitor (attempt %d/%d): %v",
				attempt+1, BMSMaxRetryTakeInhibitor, err))
		}

		if attempt < BMSMaxRetryTakeInhibitor-1 {
			time.Sleep(1 * time.Millisecond)
		}
	}

	r.logger.Error(fmt.Sprintf("Failed to acquire suspend inhibitor after %d attempts: %v",
		BMSMaxRetryTakeInhibitor, lastErr))
}

func (r *BatteryReader) releaseInhibitor() {
	if r.suspendInhibitor == nil {
		return
	}

	r.logger.Debug(fmt.Sprintf("Releasing inhibitor"))

	if err := r.suspendInhibitor.Release(); err != nil {
		r.logger.Warn(fmt.Sprintf("Failed to release inhibitor: %v",err))
	}

	r.suspendInhibitor = nil
}

func (r *BatteryReader) clearHeartbeatTimer() {
	if r.heartbeatTimer != nil {
		r.heartbeatTimer.Stop()
		r.heartbeatTimer = nil
	}
	r.heartbeatRunning = false
}
