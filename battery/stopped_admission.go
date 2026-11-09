package battery

import "time"

const (
	// stoppedMotionMaxAge bounds how old a sample may be when an ON decision
	// consults it. The decision takes its own sample first, so in practice it is
	// never older than the Redis round trip.
	stoppedMotionMaxAge = 500 * time.Millisecond
	// ecuSampleMaxAge is how long a stopped sample keeps counting as current
	// between the poller's samples.
	ecuSampleMaxAge = 2 * admissionPollInterval
)

// stoppedAdmission holds the latest ECU motion sample. Samples come from the
// ECU's published state when a decision needs them, not from a live stream.
type stoppedAdmission struct {
	at      time.Time
	stopped bool
	// since is when an unbroken run of stopped samples began.
	since time.Time
}

func (a *packAdmission) observeMotion(stopped bool, received, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observeMotionLocked(stopped, received, now)
}

func (a *packAdmission) observeMotionLocked(stopped bool, received, now time.Time) {
	m := &a.motion
	if received.IsZero() || received.After(now) || now.Sub(received) > stoppedMotionMaxAge {
		*m = stoppedAdmission{}
		return
	}
	// Movement or a throttle report discards measurements taken before it.
	if !m.at.IsZero() && m.stopped != stopped {
		a.cancelRoundLocked("motion qualification changed")
		for i := range a.slots {
			a.slots[i].history = nil
		}
	}
	switch {
	case !stopped:
		m.since = time.Time{}
	case !m.stopped || m.since.IsZero() || received.Sub(m.at) > ecuSampleMaxAge:
		m.since = received
	}
	m.at, m.stopped = received, stopped
}

func (a *packAdmission) invalidateMotion() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.motion = stoppedAdmission{}
}

// stationaryLocked reports whether a second supply may be read and admitted.
// A fresh movement or throttle report always vetoes. In a known non-drive state
// the vehicle state itself is the evidence, once it has held for a moment. In
// ready-to-drive the ECU must have reported standstill continuously for that
// long; missing ECU data is not a zero-speed reading. Any other state blocks.
func (a *packAdmission) stationaryLocked(now time.Time) bool {
	m := &a.motion
	fresh := !m.at.IsZero() && !now.Before(m.at) && now.Sub(m.at) <= ecuSampleMaxAge
	if fresh && !m.stopped {
		return false
	}
	switch {
	case nonDrivingVehicle(a.vehicle):
		return now.Sub(a.vehicleSince) >= admissionStillTime
	case a.vehicle == VehicleStateReadyToDrive:
		return fresh && !m.since.IsZero() && now.Sub(m.since) >= admissionStillTime
	}
	return false
}

// wantsStandstillSampleLocked reports whether the ECU should be sampled for
// standstill because an idle pack could join once the vehicle has stood still.
// Only ready-to-drive needs it; non-drive states are stationary by themselves.
func (a *packAdmission) wantsStandstillSampleLocked(now time.Time) bool {
	if a.vehicle != VehicleStateReadyToDrive || a.sampling.stopped {
		return false
	}
	_, needsStill := a.candidateStateLocked(now)
	return needsStill
}

func (a *packAdmission) wantsStandstillSample(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.wantsStandstillSampleLocked(now)
}
