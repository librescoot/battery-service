package battery

import (
	"fmt"
	"strings"
	"time"
)

// A preview owns all mutable state, including slices and the in-flight round.
// Each candidate starts from the same snapshot; previews never reserve power.
func (a *packAdmission) cloneLocked() *packAdmission {
	copy := &packAdmission{coordinated: a.coordinated, conservative: a.conservative,
		vehicle: a.vehicle, slots: a.slots, sampling: a.sampling, motion: a.motion}
	for i := range copy.slots {
		copy.slots[i].history = append([]admissionSample(nil), a.slots[i].history...)
	}
	if a.sampling.round != nil {
		round := *a.sampling.round
		copy.sampling.round = &round
	}
	if a.isolation != nil {
		episode := *a.isolation
		copy.isolation = &episode
	}
	return copy
}

func (a *packAdmission) previewOn(index int, now time.Time, maxDelta uint64, joining, conservative bool) (bool, string) {
	a.mu.Lock()
	copy := a.cloneLocked()
	a.mu.Unlock()
	copy.conservative = conservative
	if joining {
		copy.slots[index].held, copy.slots[index].active = false, false
		copy.slots[index].onAt = time.Time{}
	}
	return copy.requestOn(index, now, maxDelta)
}

func evidenceAge(now, observed time.Time) string {
	if observed.IsZero() {
		return "unknown"
	}
	return now.Sub(observed).Round(time.Millisecond).String()
}

func (a *packAdmission) diagnostics(now time.Time, maxDelta uint64) (string, string) {
	a.mu.Lock()
	copy := a.cloneLocked()
	a.mu.Unlock()
	for _, s := range copy.slots {
		if !s.haveFrame || !s.frame.Present {
			return "", ""
		}
	}
	var text, key strings.Builder
	fmt.Fprintf(&text, "Dual-pack decision: vehicle=%s conservative=%t delta_mv=%d limit_mv=%d paired_round=%d pair_age=%s",
		copy.vehicle, copy.conservative, voltageDistance(copy.slots[0].frame.Voltage, copy.slots[1].frame.Voltage),
		maxDelta, copy.sampling.pairedID, evidenceAge(now, copy.sampling.pairedAt))
	fmt.Fprintf(&key, "%s/%t/%d", copy.vehicle, copy.conservative, maxDelta)
	if e := copy.isolation; e != nil {
		fmt.Fprintf(&text, " target=%d victim=%d handover=%t off_attempted=%t off_confirmed=%t", e.survivor, e.victim, e.handover, e.attempted, e.offConfirmed)
		fmt.Fprintf(&key, "/isolation:%d:%t:%t:%t", e.victim, e.handover, e.attempted, e.offConfirmed)
	}
	for i, s := range copy.slots {
		on, reason := copy.previewOn(i, now, maxDelta, false, copy.conservative)
		join, joinReason := copy.previewOn(i, now, maxDelta, true, false)
		strict, strictReason := copy.previewOn(i, now, maxDelta, true, true)
		fmt.Fprintf(&text, " slot%d={state=%s eligible=%t absent=%t voltage_mv=%d current_ma=%d charge=%d fault=%d fresh=%t read_age=%s changed_age=%s round=%d samples=%d stable=%t held=%t active_confirmed=%t off_pending=%t on_age=%s last_on_age=%s off_age=%s off_after_last_on=%t command_on=%t reason=%q baseline_new_on=%t baseline_reason=%q strict_new_on=%t strict_reason=%q}",
			i, s.frame.State, s.eligible, s.absent, s.frame.Voltage, s.frame.Current, s.frame.Charge, s.frame.FaultCode,
			s.fresh(now), evidenceAge(now, s.observed), evidenceAge(now, s.written), s.observedRound, len(s.history), s.stable(now),
			s.held, s.active, s.offPending, evidenceAge(now, s.onAt), evidenceAge(now, s.lastOnAt), evidenceAge(now, s.offAt), !s.offAt.Before(s.lastOnAt), on, reason, join, joinReason, strict, strictReason)
		fmt.Fprintf(&key, "/%s:%t:%s:%t:%s:%t:%s", s.frame.State, on, reason, join, joinReason, strict, strictReason)
	}
	return text.String(), key.String()
}

func (s *Service) logAdmissionDiagnostics(now time.Time) {
	message, key := s.admission.diagnostics(now, s.config.MaxVoltageDelta.Load())
	if message == "" {
		return
	}
	if key != s.admissionLogKey || now.Sub(s.admissionLogAt) >= 30*time.Second {
		s.logger.Info(message)
		s.admissionLogKey, s.admissionLogAt = key, now
	} else {
		s.logger.Debug(message)
	}
}
