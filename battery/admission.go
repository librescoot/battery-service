package battery

import (
	"sync"
	"time"
)

// These conservative admission bounds are independent of the BMS heartbeat.
// Current is signed: charging and discharging both count as load.
const (
	admissionPollInterval = 3 * time.Second
	admissionMaxAge       = 10 * time.Second
	admissionMaxSkew      = 3 * time.Second
	admissionStableTime   = 6 * time.Second
	admissionMaxCurrent   = 1000 // mA
	admissionVoltageSpan  = 100  // mV per pack over the stable window
	admissionOnSettle     = 10 * time.Second
)

type admissionSample struct {
	data BMSData
	at   time.Time
}

type admissionSlot struct {
	monitored     bool
	eligible      bool
	uid           string
	absent        bool
	observed      time.Time
	observedRound uint64
	frame         BMSData
	haveFrame     bool
	written       time.Time
	valid         bool
	history       []admissionSample

	// A command reservation counts as a possible live supply even if its
	// write fails. Only positive OFF evidence may release it; a read error
	// or an NFC departure is not evidence that power stopped flowing.
	held              bool
	active            bool
	onAt              time.Time
	lastOnAt          time.Time
	offPending        bool
	offAt             time.Time
	identityUncertain bool
}

// packAdmission serializes the readers' decisions without holding a lock
// across NFC or Redis I/O. Reserving a slot precedes its first ON write.
// Once dual operation has been enabled, protection remains engaged even
// when its setting is disabled: an OFF command still needs confirmation.
type packAdmission struct {
	mu           sync.Mutex
	coordinated  bool
	conservative bool
	isolation    *isolationEpisode
	vehicle      VehicleState
	slots        [2]admissionSlot
	sampling     samplingRounds
	motion       stoppedAdmission
}

func newPackAdmission(config *BatteryConfiguration) *packAdmission {
	a := &packAdmission{conservative: true}
	for _, r := range config.Readers {
		if r.Index < 0 || r.Index >= len(a.slots) {
			continue
		}
		a.slots[r.Index].monitored = r.Enabled
		a.setRole(r.Index, r.Enabled && r.Role == BatteryRoleActive)
	}
	a.coordinated = a.slots[0].monitored && a.slots[1].monitored
	return a
}

func (a *packAdmission) setRole(index int, eligible bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelRoundLocked("role changed")
	a.slots[index].eligible = eligible
	if a.slots[0].eligible && a.slots[1].eligible {
		a.coordinated = true
	}
	for i := range a.slots {
		a.slots[i].history = nil
	}
}

func (a *packAdmission) setVehicle(state VehicleState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.vehicle != state {
		a.cancelRoundLocked("vehicle changed")
		for i := range a.slots {
			a.slots[i].history = nil
		}
	}
	a.vehicle = state
}

func (a *packAdmission) tag(index int, uid []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelRoundLocked("tag changed")
	s := &a.slots[index]
	if s.uid != string(uid) {
		// A new tag invalidates permission, not an unresolved power command.
		*s = admissionSlot{monitored: s.monitored, eligible: s.eligible, uid: string(uid),
			held: s.held, onAt: s.onAt, lastOnAt: s.lastOnAt, offPending: s.offPending, offAt: s.offAt, identityUncertain: s.held}
	} else {
		s.valid = false
		s.haveFrame = false
		s.written = time.Time{}
		s.history = nil
	}
	s.absent = false
}

func (a *packAdmission) invalidate(index int) {
	a.failAcquisition(index, 0, acquisition{})
}

func (a *packAdmission) failAcquisition(index int, id uint64, sample acquisition) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &a.slots[index]
	if r := a.sampling.round; r != nil && r.id == id {
		r.samples[index] = sample
	}
	a.cancelRoundLocked("read failed")
	s.valid = false
	s.absent = false
	s.history = nil
}

func (a *packAdmission) observeAbsent(index int, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &a.slots[index]
	s.valid = false
	s.history = nil
	a.cancelRoundLocked("pack absent")
	s.absent = !s.held
	s.observed = now
}

// Only complete, unsubstituted NFC reads renew acquisition freshness. Stable
// values remain valid; payload changes are separate evidence for releasing
// uncertain commands. Cached Redis values never participate in admission.
func (a *packAdmission) observe(index int, data BMSData, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observeLocked(index, data, now)
}

func (a *packAdmission) observeLocked(index int, data BMSData, now time.Time) {
	s := &a.slots[index]
	s.observedRound = 0
	if s.haveFrame && statusFrameChanged(s.frame, data) {
		s.written = now
	}
	s.frame, s.haveFrame, s.observed, s.absent = data, true, now, false
	s.valid = data.Present && data.Voltage > 0 &&
		(data.State == BMSStateActive || data.State == BMSStateIdle || data.State == BMSStateAsleep)
	if !s.valid {
		s.history = nil
		return
	}

	if data.State == BMSStateActive {
		s.held, s.active, s.identityUncertain = true, true, false
		if e := a.isolation; e != nil && e.handover && e.offConfirmed && e.survivor == index {
			a.isolation = nil
		}
	} else if (!s.offPending && s.written.After(s.lastOnAt) && ((s.active && now.Sub(s.lastOnAt) >= BMSTimeCmd) || now.Sub(s.lastOnAt) >= admissionOnSettle)) ||
		(s.offPending && !s.offAt.Before(s.lastOnAt) && now.Sub(s.offAt) >= BMSTimeCmd && s.written.After(s.offAt)) {
		if s.held {
			a.cancelRoundLocked("supply became inactive")
			for i := range a.slots {
				a.slots[i].history = nil
			}
		}
		s.held, s.active, s.offPending, s.identityUncertain = false, false, false, false
		if e := a.isolation; e != nil {
			if e.victim == index {
				if e.handover {
					e.offConfirmed = true
				} else {
					a.isolation = nil
				}
			} else if !e.handover && e.survivor == index && !e.attempted {
				a.isolation = nil
			}
		}
	}

	if a.sampling.enabled && a.coordinated {
		return
	}
	s.appendHistory(data, now)
}

func (s *admissionSlot) appendHistory(data BMSData, now time.Time) {
	if len(s.history) > 0 && now.Sub(s.history[len(s.history)-1].at) > admissionMaxAge {
		s.history = nil
	}
	s.history = append(s.history, admissionSample{data: data, at: now})
	// Keep at least three samples and the sample just before the window
	// boundary, including when discovery lengthens the sampling cadence.
	for len(s.history) > 3 && now.Sub(s.history[1].at) >= admissionStableTime {
		s.history = s.history[1:]
	}
}

func (s *admissionSlot) fresh(now time.Time) bool {
	return s.valid && now.Sub(s.observed) <= admissionMaxAge
}

func (s *admissionSlot) usable() bool {
	return s.eligible && s.frame.FaultCode == 0 && !(s.frame.LowSOC && s.frame.Charge == 0)
}

func (s *admissionSlot) stable(now time.Time) bool {
	if !s.fresh(now) || len(s.history) < 3 || s.history[len(s.history)-1].at.Sub(s.history[0].at) < admissionStableTime {
		return false
	}
	minV, maxV := s.frame.Voltage, s.frame.Voltage
	for _, sample := range s.history {
		if sample.data.Current < -admissionMaxCurrent || sample.data.Current > admissionMaxCurrent {
			return false
		}
		minV = min(minV, sample.data.Voltage)
		maxV = max(maxV, sample.data.Voltage)
	}
	return maxV-minV <= admissionVoltageSpan
}

func voltageDistance(v0, v1 uint) uint64 {
	if v0 > v1 {
		return uint64(v0 - v1)
	}
	return uint64(v1 - v0)
}

func (a *packAdmission) requestOn(index int, now time.Time, maxDelta uint64) (allowed bool, reason string) {
	a.mu.Lock()
	defer func() {
		if allowed {
			a.slots[index].lastOnAt = now
		}
		a.mu.Unlock()
	}()
	a.reconcileLocked(now, maxDelta)
	s, peer := &a.slots[index], &a.slots[1-index]
	if !s.eligible {
		return false, "inactive role"
	}
	if s.offPending {
		if a.maintainHandoverLocked(index, now) {
			return true, "maintaining sole supply; handover paused while driving"
		}
		return false, "waiting for OFF confirmation"
	}
	if e := a.isolation; e != nil && e.victim == index &&
		(e.attempted || a.isolationReadyLocked(now) || !s.held || !s.active) {
		return false, "isolating mismatched running pack"
	}
	if e := a.isolation; e != nil && e.handover && e.survivor == index && !e.offConfirmed {
		return false, "waiting for lower-voltage pack OFF confirmation"
	}
	if s.identityUncertain && s.held && !s.active {
		return false, "waiting for tag power confirmation"
	}
	if !a.coordinated && !peer.held && !peer.offPending {
		// Track commands even in single mode: enabling the second slot can
		// race the first pack's ON confirmation.
		if !s.held || s.offPending {
			s.held, s.active, s.onAt, s.offPending = true, false, now, false
		}
		return true, "single-battery mode"
	}
	if s.held && s.active {
		return true, "maintaining supply"
	}
	switch a.vehicle {
	case VehicleStateParked, VehicleStateStandby, VehicleStateWaitingSeatbox:
	case VehicleStateReadyToDrive:
		if !a.motion.enabled {
			return false, "waiting for activation vehicle state"
		}
		if !peer.held || !peer.active || peer.offPending {
			return false, "waiting for running peer"
		}
		if !a.stoppedPairReadyLocked(now) {
			return false, "waiting for fresh stopped-vehicle pair"
		}
	default:
		return false, "waiting for activation vehicle state"
	}
	if s.held {
		if now.Sub(s.onAt) > admissionOnSettle {
			return false, "waiting for ON confirmation"
		}
	}
	if !s.fresh(now) {
		return false, "waiting for fresh pack data"
	}
	if !s.usable() {
		return false, "pack empty or faulted"
	}
	if peer.monitored {
		absent := peer.absent && now.Sub(peer.observed) <= admissionMaxAge
		if !absent && !peer.fresh(now) {
			return false, "waiting for fresh peer data"
		}
		if peer.held {
			if !peer.active || peer.offPending {
				return false, "waiting for peer power confirmation"
			}
			if peer.frame.FaultCode != 0 || (peer.frame.LowSOC && peer.frame.Charge == 0) {
				return false, "peer empty or faulted"
			}
			if a.vehicle != VehicleStateParked && a.vehicle != VehicleStateReadyToDrive {
				return false, "waiting for parked"
			}
			if a.sampling.enabled && (a.sampling.pairedAt.IsZero() || now.Sub(a.sampling.pairedAt) > admissionMaxAge ||
				s.observedRound != a.sampling.pairedID || peer.observedRound != a.sampling.pairedID) {
				return false, "waiting for paired acquisition"
			}
			skew := s.observed.Sub(peer.observed)
			if skew < -admissionMaxSkew || skew > admissionMaxSkew {
				return false, "waiting for aligned readings"
			}
			if maxDelta > 0 && voltageDistance(s.frame.Voltage, peer.frame.Voltage) > maxDelta {
				return false, "voltage difference too large"
			}
			if a.conservative {
				if !s.stable(now) || !peer.stable(now) {
					return false, "waiting for stable low-load readings"
				}
				if maxDelta > 0 {
					limit := maxDelta - min(uint64(100), maxDelta/10)
					for _, own := range s.history {
						for _, other := range peer.history {
							if voltageDistance(own.data.Voltage, other.data.Voltage) > limit {
								return false, "voltage difference too large"
							}
						}
					}
				}
			}
		} else if !absent && peer.usable() {
			if !a.conservative {
				if index > 0 {
					return false, "primary slot selected"
				}
			} else if peer.frame.Voltage > s.frame.Voltage ||
				(peer.frame.Voltage == s.frame.Voltage && peer.frame.Charge > s.frame.Charge) ||
				(peer.frame.Voltage == s.frame.Voltage && peer.frame.Charge == s.frame.Charge && index > 0) {
				return false, "higher-voltage pack selected"
			}
		}
	}
	if s.held {
		return true, "awaiting ON confirmation"
	}
	a.cancelRoundLocked("activation reserved")
	s.held, s.onAt = true, now
	// Samples before a command cannot establish its settled load.
	for i := range a.slots {
		a.slots[i].history = nil
	}
	return true, "activation admitted"
}

func (a *packAdmission) offAttempt(index int, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.offAttemptLocked(index, now)
}

func (a *packAdmission) offAttemptLocked(index int, now time.Time) {
	if e := a.isolation; e != nil && e.victim == index {
		e.attempted = true
	}
	a.cancelRoundLocked("OFF command")
	s := &a.slots[index]
	if s.held && (!s.offPending || s.lastOnAt.After(s.offAt)) {
		s.offPending, s.offAt = true, now
	}
	for i := range a.slots {
		a.slots[i].history = nil
	}
}

func (a *packAdmission) waiting(index int, state BMSState) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &a.slots[index]
	// A deferred ON cannot require an active response. Freshness still gates
	// requestOn; an outstanding supply reservation still requires recovery.
	return a.coordinated && !s.held && !s.offPending &&
		(state == BMSStateIdle || state == BMSStateAsleep)
}

func (a *packAdmission) needsPoll() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.needsPollLocked()
}

func (a *packAdmission) needsPollLocked() bool {
	if !a.coordinated {
		return false
	}
	if a.sampling.enabled {
		return a.slots[0].monitored && a.slots[1].monitored && (a.slots[0].eligible || a.slots[1].eligible)
	}
	switch a.vehicle {
	case VehicleStateParked:
	case VehicleStateStandby, VehicleStateWaitingSeatbox:
		for _, s := range a.slots {
			if s.active {
				return false
			}
		}
	default:
		return false
	}
	for _, s := range a.slots {
		if s.monitored && s.eligible && ((!s.active && !s.absent) || s.offPending) {
			return true
		}
	}
	return false
}
