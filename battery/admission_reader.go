package battery

import (
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
	a := r.packAdmission()
	if a == nil {
		return true
	}
	switch cmd {
	case fsm.BMSCmdOn:
		ok, reason := a.requestOn(r.index, now, r.service.config.MaxVoltageDelta.Load())
		if reason != r.lastAdmissionReason {
			r.logger.Info("Battery activation decision", "allowed", ok, "reason", reason)
			r.lastAdmissionReason = reason
		}
		return ok
	case fsm.BMSCmdOff:
		a.offAttempt(r.index, now)
	}
	return true
}
