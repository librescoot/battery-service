package battery

import (
	"fmt"
	"time"

	"battery-service/battery/fsm"
)

func (r *BatteryReader) packAdmission() *packAdmission {
	if r.service == nil || r.index < 0 || r.index > 1 {
		return nil
	}
	return r.service.admission
}

// admitCommand runs under nfcMu and is the final gate for every ON path.
func (r *BatteryReader) admitCommand(cmd fsm.BMSCommand, now time.Time) bool {
	return r.admitCommandPolicy(cmd, now, false)
}

func (r *BatteryReader) admitCommandPolicy(cmd fsm.BMSCommand, now time.Time, correctiveOff bool) bool {
	a := r.packAdmission()
	if a == nil {
		return true
	}
	switch cmd {
	case fsm.BMSCmdOn:
		ok, reason := a.requestOn(r.index, now, r.service.config.MaxVoltageDelta.Load())
		if reason != r.lastAdmissionReason {
			r.service.logger.Info(fmt.Sprintf("Battery %d activation decision: allowed=%t reason=%q", r.index, ok, reason))
			r.lastAdmissionReason = reason
		}
		return ok
	case fsm.BMSCmdOff:
		if correctiveOff {
			if !a.isolationOffAttempt(r.index, now) {
				const reason = "corrective OFF deferred: vehicle state or power evidence unavailable"
				if r.lastAdmissionReason != reason {
					r.service.logger.Warn(fmt.Sprintf("Battery %d %s", r.index, reason))
					r.lastAdmissionReason = reason
				}
				return false
			}
			if r.lastAdmissionReason != "corrective OFF authorized" {
				r.service.logger.Info(fmt.Sprintf("Battery %d corrective OFF authorized; awaiting inactive confirmation", r.index))
				r.lastAdmissionReason = "corrective OFF authorized"
			}
		} else {
			a.offAttempt(r.index, now)
		}
	}
	return true
}
