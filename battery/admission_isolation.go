package battery

import "time"

type isolationEpisode struct {
	survivor, victim int
	attempted        bool
	handover         bool
	offConfirmed     bool
}

func isolationVehicle(state VehicleState) bool {
	return nonDrivingVehicle(state)
}

// Select once from a fresh pair of already-running packs. A later voltage
// crossover must not cause the two reader goroutines to switch both packs off.
func (a *packAdmission) reconcileLocked(now time.Time, maxDelta uint64) {
	if e := a.isolation; e != nil {
		if !e.attempted && maxDelta == 0 {
			a.isolation = nil
			return
		}
		if e.handover && !e.attempted && a.pairCurrentLocked(now) && a.slots[0].fresh(now) && a.slots[1].fresh(now) {
			target, old := &a.slots[e.survivor], &a.slots[e.victim]
			if !target.usable() || target.frame.Voltage <= old.frame.Voltage || maxDelta == 0 || voltageDistance(target.frame.Voltage, old.frame.Voltage) <= maxDelta {
				a.isolation = nil
				return
			}
		}
		if e.handover && e.offConfirmed && !a.slots[e.survivor].held && !a.slots[e.victim].held {
			target, old := &a.slots[e.survivor], &a.slots[e.victim]
			absent := target.absent && now.Sub(target.observed) <= admissionMaxAge
			if absent || !target.usable() || (old.usable() && old.frame.Voltage > target.frame.Voltage) {
				a.isolation = nil
			}
		}
		return
	}
	if !a.coordinated || maxDelta == 0 || !isolationVehicle(a.vehicle) {
		return
	}
	for _, s := range a.slots {
		if s.offPending || !s.fresh(now) {
			return
		}
	}
	if !a.pairCurrentLocked(now) || voltageDistance(a.slots[0].frame.Voltage, a.slots[1].frame.Voltage) <= maxDelta {
		return
	}
	survivor := 0
	if a.slots[1].frame.Voltage > a.slots[0].frame.Voltage {
		survivor = 1
	}
	if !a.slots[survivor].usable() {
		survivor = 1 - survivor
		if !a.slots[survivor].usable() {
			return
		}
	}
	target, victim := &a.slots[survivor], &a.slots[1-survivor]
	if !victim.held || !victim.active || victim.frame.State != BMSStateActive {
		return
	}
	if target.held && target.active && target.frame.State == BMSStateActive {
		a.isolation = &isolationEpisode{survivor: survivor, victim: 1 - survivor}
		return
	}
	if a.conservative && !target.held && (target.frame.State == BMSStateIdle || target.frame.State == BMSStateAsleep) &&
		target.stable(now) && victim.stable(now) {
		a.isolation = &isolationEpisode{survivor: survivor, victim: 1 - survivor, handover: true}
	}
}

func (a *packAdmission) pairCurrentLocked(now time.Time) bool {
	skew := a.slots[0].observed.Sub(a.slots[1].observed)
	if skew < -admissionMaxSkew || skew > admissionMaxSkew {
		return false
	}
	return !a.sampling.enabled || (!a.sampling.pairedAt.IsZero() && now.Sub(a.sampling.pairedAt) <= admissionMaxAge &&
		a.slots[0].observedRound == a.sampling.pairedID && a.slots[1].observedRound == a.sampling.pairedID)
}

func (a *packAdmission) isolationReadyLocked(now time.Time) bool {
	e := a.isolation
	if e == nil || !isolationVehicle(a.vehicle) {
		return false
	}
	s := &a.slots[e.survivor]
	if e.handover {
		if e.offConfirmed || s.held || s.offPending || !s.fresh(now) || !s.usable() ||
			(s.frame.State != BMSStateIdle && s.frame.State != BMSStateAsleep) {
			return false
		}
		return e.attempted || (a.pairCurrentLocked(now) && s.stable(now) && a.slots[e.victim].stable(now))
	}
	return s.held && s.active && !s.offPending && s.fresh(now) && s.usable() && s.frame.State == BMSStateActive
}

func (a *packAdmission) shouldIsolate(index int, now time.Time, maxDelta uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reconcileLocked(now, maxDelta)
	e := a.isolation
	if a.maintainHandoverLocked(index, now) {
		return false
	}
	return e != nil && e.victim == index && !e.offConfirmed && (e.attempted || a.isolationReadyLocked(now))
}

// Pausing a handover must not starve the still-running sole supply of
// heartbeats. The OFF reservation remains held, and a later OFF must follow
// this keepalive before any inactive observation can authorize the target.
func (a *packAdmission) maintainHandoverLocked(index int, now time.Time) bool {
	e := a.isolation
	if e == nil || !e.handover || !e.attempted || e.offConfirmed || e.victim != index || a.vehicle != VehicleStateReadyToDrive {
		return false
	}
	old, target := &a.slots[e.victim], &a.slots[e.survivor]
	targetInactive := (target.absent && now.Sub(target.observed) <= admissionMaxAge) ||
		(target.fresh(now) && (target.frame.State == BMSStateIdle || target.frame.State == BMSStateAsleep))
	return old.held && old.active && old.fresh(now) && old.frame.State == BMSStateActive &&
		old.observed.After(old.offAt) && now.Sub(old.offAt) >= BMSTimeCmd &&
		!target.held && !target.offPending && targetInactive
}

// This runs before every NFC write attempt, including retries. An uncertain
// OFF stays reserved, but losing the survivor's evidence forbids another OFF.
func (a *packAdmission) isolationOffAttempt(index int, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.isolation
	if e == nil || e.victim != index {
		return false
	}
	if !a.isolationReadyLocked(now) {
		return false
	}
	e.attempted = true
	a.offAttemptLocked(index, now)
	return true
}
