package battery

import (
	"fmt"
	"time"
)

// inhibitorBackoff is the exponential schedule between retries. Total budget
// before falling back is ~1.85s of sleep + up to 5×3s of dbus call timeouts =
// ~17s worst case before takeInhibitor returns.
var inhibitorBackoff = []time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
}

func (r *BatteryReader) takeInhibitor() error {
	if r.suspendInhibitor != nil && r.suspendInhibitor.IsActive() {
		r.logger.Debug("Inhibitor already acquired")
		return nil
	}

	attempts := len(inhibitorBackoff) + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		inhibitor, err := NewSuspendInhibitor(
			"BATTERY_NFC_TRANSACTION_INHIBITOR",
			"NFC_TRANSACTION",
			"block",
		)
		if err == nil {
			r.suspendInhibitor = inhibitor
			r.logger.Debug("Inhibitor acquired")
			return nil
		}

		lastErr = err
		if attempt == 0 {
			r.logger.Debug(fmt.Sprintf("Failed to acquire suspend inhibitor (attempt %d/%d): %v",
				attempt+1, attempts, err))
		} else {
			r.logger.Warn(fmt.Sprintf("Failed to acquire suspend inhibitor (attempt %d/%d): %v",
				attempt+1, attempts, err))
		}

		if attempt < len(inhibitorBackoff) {
			time.Sleep(inhibitorBackoff[attempt])
		}
	}

	return fmt.Errorf("after %d attempts: %w", attempts, lastErr)
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
