package battery

import "time"

const (
	stoppedAdmissionSetting = "experimental.battery.dual-pack-admission-while-stopped"
	stoppedMotionMaxAge     = 500 * time.Millisecond
	stoppedPairMaxAge       = admissionPollInterval
)

type stoppedAdmission struct {
	enabled   bool
	at        time.Time
	since     time.Time
	stopped   bool
	source    string
	sequence  uint64
	lastCheck time.Time
}

// ECU observation timestamps, not Redis delivery time or cached hash values,
// establish liveness. Gaps and movement/throttle reports restart qualification.
func (a *packAdmission) observeMotion(stopped bool, received, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observeMotionLocked(stopped, received, now)
}

func (a *packAdmission) observeMotionLocked(stopped bool, received, now time.Time) {
	m := &a.motion
	if motionClockJump(m.lastCheck, now) {
		a.invalidateMotionLocked()
	}
	m.lastCheck = now
	if received.IsZero() || received.After(now) || now.Sub(received) > stoppedMotionMaxAge ||
		(!m.at.IsZero() && !received.After(m.at)) {
		m.at, m.since, m.stopped = time.Time{}, time.Time{}, false
		return
	}
	if m.at.IsZero() || m.stopped != stopped {
		a.cancelRoundLocked("motion qualification changed")
		for i := range a.slots {
			a.slots[i].history = nil
		}
	}
	if !stopped {
		m.since = time.Time{}
	} else if !m.stopped || received.Sub(m.at) > stoppedMotionMaxAge {
		m.since = received
	}
	m.at, m.stopped = received, stopped
}

func (a *packAdmission) invalidateMotion() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invalidateMotionLocked()
}

func (a *packAdmission) invalidateMotionLocked() {
	a.motion.at, a.motion.since, a.motion.stopped = time.Time{}, time.Time{}, false
}

func motionClockJump(previous, now time.Time) bool {
	if previous.IsZero() {
		return false
	}
	delta := time.Duration(now.UnixNano()-previous.UnixNano()) - now.Sub(previous)
	return delta > 5*time.Millisecond || delta < -5*time.Millisecond
}

func (a *packAdmission) secondPackStationaryLocked(now time.Time) bool {
	if nonDrivingVehicle(a.vehicle) {
		// A fresh movement/throttle report overrides the stationary state.
		m := &a.motion
		return m.at.IsZero() || now.Before(m.at) || now.Sub(m.at) > stoppedMotionMaxAge || m.stopped
	}
	return a.stoppedPairReadyLocked(now)
}

func (a *packAdmission) stoppedMotionFreshLocked(now time.Time) bool {
	m := &a.motion
	if motionClockJump(m.lastCheck, now) {
		a.invalidateMotionLocked()
		return false
	}
	return m.enabled && m.stopped && !m.since.IsZero() && !now.Before(m.at) && now.Sub(m.at) <= stoppedMotionMaxAge
}

func (a *packAdmission) stoppedPairReadyLocked(now time.Time) bool {
	if !a.stoppedMotionFreshLocked(now) {
		return false
	}
	m := &a.motion
	p := &a.sampling
	if !p.enabled || p.stopped || p.pairedID == 0 || p.pairedAt.Before(m.since) ||
		now.Before(p.pairedAt) || now.Sub(p.pairedAt) > stoppedPairMaxAge {
		return false
	}
	for _, s := range a.slots {
		// A post-read ECU report confirms the stopped/throttle-off condition
		// still holds. All stability samples must belong to this stop.
		if s.observedRound != p.pairedID || m.at.Before(s.observed) ||
			len(s.history) == 0 || s.history[0].at.Before(m.since) {
			return false
		}
	}
	return true
}
