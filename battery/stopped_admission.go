package battery

import "time"

// stoppedMotionMaxAge bounds how old an ECU sample may be when a second-pack
// decision consults it. Samples are taken at decision time, so in practice they
// are never older than the Redis round trip.
const stoppedMotionMaxAge = 500 * time.Millisecond

// stoppedAdmission holds the latest ECU motion sample. Samples come from the
// ECU's published state when a decision needs them, not from a live stream.
type stoppedAdmission struct {
	at      time.Time
	stopped bool
}

func (a *packAdmission) observeMotion(stopped bool, received, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observeMotionLocked(stopped, received, now)
}

func (a *packAdmission) observeMotionLocked(stopped bool, received, now time.Time) {
	m := &a.motion
	if received.IsZero() || received.After(now) || now.Sub(received) > stoppedMotionMaxAge {
		m.at, m.stopped = time.Time{}, false
		return
	}
	// Movement or a throttle report discards measurements taken before it.
	if !m.at.IsZero() && m.stopped != stopped {
		a.cancelRoundLocked("motion qualification changed")
		for i := range a.slots {
			a.slots[i].history = nil
		}
	}
	m.at, m.stopped = received, stopped
}

func (a *packAdmission) invalidateMotion() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.motion.at, a.motion.stopped = time.Time{}, false
}

// Missing or stale motion data is not a zero-speed reading, but it does not
// block either: only a fresh movement or throttle report overrides a known
// non-drive vehicle state. A second supply is never added in a drive state.
func (a *packAdmission) secondPackStationaryLocked(now time.Time) bool {
	if !nonDrivingVehicle(a.vehicle) {
		return false
	}
	m := &a.motion
	return m.at.IsZero() || now.Before(m.at) || now.Sub(m.at) > stoppedMotionMaxAge || m.stopped
}
