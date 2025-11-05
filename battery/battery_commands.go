package battery

import (
	"fmt"
	"time"
)

func (r *BatteryReader) takeInhibitor() {
	if r.suspendInhibitor != nil && r.suspendInhibitor.IsActive() {
		r.logger.Debug("Inhibitor already acquired")
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
			r.logger.Debug("Inhibitor acquired")
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

	r.logger.Debug("Releasing inhibitor")

	if err := r.suspendInhibitor.Release(); err != nil {
		r.logger.Warn(fmt.Sprintf("Failed to release inhibitor: %v", err))
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
