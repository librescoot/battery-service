package battery

import (
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	stoppedAdmissionSetting = "experimental.battery.dual-pack-admission-while-stopped"
	stoppedMotionMaxAge     = 500 * time.Millisecond
	stoppedPairMaxAge       = admissionPollInterval
)

func (s *Service) loadStoppedAdmissionSetting() {
	if s.admission == nil {
		return
	}
	raw, err := s.redis.HGet(s.ctx, "settings", stoppedAdmissionSetting).Result()
	if err != nil {
		if err != redis.Nil {
			s.logger.Warn(fmt.Sprintf("Failed to load %s: %v", stoppedAdmissionSetting, err))
		}
		return
	}
	enabled, valid := parseBoolSetting(raw)
	if !valid {
		s.logger.Warn(fmt.Sprintf("Invalid %s value: %q", stoppedAdmissionSetting, raw))
		return
	}
	s.admission.motion.enabled = enabled
	s.logger.Info(fmt.Sprintf("Experimental stopped-vehicle admission: enabled=%t", enabled))
}

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

func (a *packAdmission) stoppedPairReadyLocked(now time.Time) bool {
	m := &a.motion
	if motionClockJump(m.lastCheck, now) {
		a.invalidateMotionLocked()
		return false
	}
	if !m.enabled || !m.stopped || m.since.IsZero() || now.Before(m.at) || now.Sub(m.at) > stoppedMotionMaxAge {
		return false
	}
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
