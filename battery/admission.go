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
	// admissionStillTime is how long the vehicle must have stood in a non-drive
	// admissionRetryInterval is how long a candidate that was read but not
	// admitted waits before it is read again.
	admissionRetryInterval = 30 * time.Second
	// state before a second pack is read for admission.
	admissionStillTime  = 5 * time.Second
	admissionMaxCurrent = 1000 // mA
	admissionOnSettle   = 10 * time.Second
)

type admissionSample struct {
	data BMSData
	at   time.Time
}

type admissionSlot struct {
	monitored bool
	eligible  bool
	// Retain power tracking across role and identity changes. A storage-only
	// reader must not gate ON until enabled or observed reporting ACTIVE.
	powerTracked  bool
	uid           string
	serial        string
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
	requiresAdmission bool
}

func (s *admissionSlot) replaceIdentity(uid string) {
	*s = admissionSlot{monitored: s.monitored, eligible: s.eligible, powerTracked: s.powerTracked, uid: uid,
		held: s.held, onAt: s.onAt, lastOnAt: s.lastOnAt, offPending: s.offPending, offAt: s.offAt,
		identityUncertain: s.held, requiresAdmission: s.requiresAdmission || s.uid != "" || s.serial != "" || s.haveFrame}
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
	vehicleSince time.Time
	slots        [2]admissionSlot
	sampling     samplingRounds
	motion       stoppedAdmission
	// maxDelta supplies the configured voltage limit; nil means the default.
	maxDelta      func() uint64
	seatboxKnown  bool
	seatboxClosed bool
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
	a.slots[index].powerTracked = a.slots[index].powerTracked || eligible
	if a.slots[0].eligible && a.slots[1].eligible {
		a.coordinated = true
	}
	for i := range a.slots {
		a.slots[i].history = nil
	}
}

func (a *packAdmission) setVehicle(state VehicleState) {
	a.setVehicleAt(state, time.Now())
}

func (a *packAdmission) setVehicleAt(state VehicleState, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.vehicle != state && !(nonDrivingVehicle(a.vehicle) && nonDrivingVehicle(state)) {
		a.vehicleSince = now
		a.cancelRoundLocked("vehicle changed")
		for i := range a.slots {
			a.slots[i].history = nil
		}
	}
	a.vehicle = state
}

func (a *packAdmission) setSeatbox(closed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.seatboxKnown {
		a.seatboxKnown, a.seatboxClosed = true, closed
		return
	}
	if a.seatboxClosed == closed {
		return
	}
	a.seatboxClosed = closed
	a.invalidateSeatboxLocked()
}

func (a *packAdmission) invalidateSeatboxLocked() {
	a.cancelRoundLocked("seatbox changed")
	for i := range a.slots {
		s := &a.slots[i]
		// Require new acquisitions without forgetting that this identity has
		// already refreshed its tag. Constant new readings remain usable.
		s.valid = false
		s.history = nil
		s.observedRound = 0
		// Reader seatbox policy issues OFF when required. Invalid measurements
		// cannot revoke an established supply's heartbeat permission.
		if !s.held || !s.active {
			s.requiresAdmission = true
		}
	}
}

func (a *packAdmission) tag(index int, uid []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelRoundLocked("tag changed")
	s := &a.slots[index]
	if s.uid != string(uid) {
		// A new tag invalidates permission, not an unresolved power command.
		s.replaceIdentity(string(uid))
	} else {
		s.valid = false
		s.haveFrame = false
		s.written = time.Time{}
		s.history = nil
		if nonDrivingVehicle(a.vehicle) {
			s.requiresAdmission = true
		}
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
	s.valid, s.haveFrame = false, false
	s.written = time.Time{}
	s.observedRound = 0
	s.history = nil
	// RF absence does not prove electrical OFF, but a reinserted pack must
	// not inherit keepalive permission, even with the same UID and serial.
	if nonDrivingVehicle(a.vehicle) {
		s.requiresAdmission = true
	}
	a.cancelRoundLocked("pack absent")
	s.absent = !s.held
	s.observed = now
}

// A newly selected tag needs one observed payload change: its memory may hold
// an old snapshot from the pack's other orientation. After that confirmation,
// complete NFC reads renew freshness even with constant values. Further changes
// are separate evidence for resolving uncertain commands.
func (a *packAdmission) observe(index int, data BMSData, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observeLocked(index, data, now)
}

func (a *packAdmission) observeLocked(index int, data BMSData, now time.Time) {
	s := &a.slots[index]
	// Even an unconfirmed ACTIVE frame prevents treating this reader as
	// storage-only; missing data afterwards cannot establish electrical OFF.
	if data.State == BMSStateActive {
		s.powerTracked = true
	}
	if s.serial != "" && data.SerialNumber != "" && s.serial != data.SerialNumber {
		a.cancelRoundLocked("pack identity changed")
		s.replaceIdentity(s.uid)
	}
	if data.SerialNumber != "" {
		s.serial = data.SerialNumber
	}
	s.observedRound = 0
	if s.haveFrame && statusFrameChanged(s.frame, data) {
		s.written = now
	}
	s.frame, s.haveFrame, s.observed, s.absent = data, true, now, false
	s.valid = data.Present && data.Voltage > 0 &&
		(data.State == BMSStateActive || data.State == BMSStateIdle || data.State == BMSStateAsleep) && !s.written.IsZero()
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

// Only the latest complete reading is kept: at rest one reading of both packs
// decides.
func (s *admissionSlot) appendHistory(data BMSData, now time.Time) {
	s.history = []admissionSample{{data: data, at: now}}
}

func (s *admissionSlot) fresh(now time.Time) bool {
	return s.valid && now.Sub(s.observed) <= admissionMaxAge
}

func (s *admissionSlot) usable() bool {
	return s.eligible && s.frame.FaultCode == 0 && !(s.frame.LowSOC && s.frame.Charge == 0)
}

// stable reports whether the latest complete reading was taken under low load.
func (s *admissionSlot) stable(now time.Time) bool {
	if !s.fresh(now) || len(s.history) == 0 {
		return false
	}
	current := s.history[len(s.history)-1].data.Current
	return current >= -admissionMaxCurrent && current <= admissionMaxCurrent
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
			a.slots[index].requiresAdmission = false
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
	// ACTIVE establishes possible power flow, not fresh admission.
	if s.held && s.active && !s.requiresAdmission {
		return true, "maintaining supply"
	}
	if a.sampling.stopped {
		return false, "service shutting down"
	}
	// An uncertain or pending peer still counts as possible parallel power.
	if !nonDrivingVehicle(a.vehicle) && (a.vehicle != VehicleStateReadyToDrive || peer.held || peer.offPending) {
		return false, "waiting for non-drive vehicle state"
	}
	if s.held && !s.active {
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
	if peer.monitored && (peer.powerTracked || peer.held || peer.offPending) {
		absent := peer.absent && now.Sub(peer.observed) <= admissionMaxAge
		if !absent && !peer.fresh(now) {
			return false, "waiting for fresh peer data"
		}
		if peer.held {
			if !a.secondPackStationaryLocked(now) {
				return false, "waiting for stationary second-pack qualification"
			}
			if !nonDrivingVehicle(a.vehicle) && peer.requiresAdmission {
				return false, "waiting for qualified running peer"
			}
			if !peer.active || peer.offPending {
				return false, "waiting for peer power confirmation"
			}
			if peer.frame.FaultCode != 0 || (peer.frame.LowSOC && peer.frame.Charge == 0) {
				return false, "peer empty or faulted"
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
			if a.conservative || !nonDrivingVehicle(a.vehicle) {
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
		if s.requiresAdmission {
			a.cancelRoundLocked("activation requalified")
			for i := range a.slots {
				a.slots[i].history = nil
			}
			return true, "pack requalified"
		}
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
	// A confirmed tag may remain inactive while ON is deferred. An unconfirmed
	// tag still needs the insertion wake-up path to refresh its cached frame.
	// Neither case bypasses requestOn or an outstanding supply reservation.
	return a.coordinated && !s.written.IsZero() && !s.held && !s.offPending &&
		(state == BMSStateIdle || state == BMSStateAsleep)
}

func (a *packAdmission) needsPoll() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.needsPollLocked(time.Now())
}

// decisionOpenLocked reports whether a paired reading could still change an
// admission or isolation decision. Rounds are demand-driven: two packs that are
// both running and already paired need no further sampling, and any event that
// invalidates the pairing (tag, role, vehicle state, OFF command) reopens the
// decision. A drive-start request polls independently of this. Heartbeats and
// their status reads continue regardless.
func (a *packAdmission) decisionOpenLocked(now time.Time) bool {
	if a.isolation != nil {
		return true
	}
	for _, s := range a.slots {
		if s.offPending || (s.eligible && !s.absent && s.requiresAdmission) {
			return true
		}
	}
	if a.slots[0].held && a.slots[0].active && a.slots[1].held && a.slots[1].active {
		return !a.sampling.settled
	}
	return a.candidateWantsRoundLocked(now)
}

// candidateWantsRoundLocked reports whether an idle pack could be admitted on
// a paired reading. An idle pack next to a running one is already read by its
// own heartbeat, so nothing is polled while the two voltages are too far apart;
// the round runs once the heartbeat readings bring them within the limit. In
// the baseline mode one paired reading decides, so no further round follows
// until an event invalidates it. Conservative qualification needs several
// paired samples and keeps sampling until the pack joins.
func (a *packAdmission) candidateWantsRoundLocked(now time.Time) bool {
	for i := range a.slots {
		c, peer := &a.slots[i], &a.slots[1-i]
		if !c.eligible || c.absent || c.held || c.offPending || c.active || a.sampling.stopped {
			continue
		}
		if !c.valid || !c.haveFrame {
			return true
		}
		if !peer.held || !peer.active || !peer.haveFrame {
			// First supply: nothing is running yet.
			return true
		}
		if a.conservative && c.frame.Voltage > peer.frame.Voltage {
			// Experimental selection can hand the supply to a higher-voltage pack.
			return true
		}
		limit := uint64(DefaultMaxVoltageDeltaMV)
		if a.maxDelta != nil {
			limit = a.maxDelta()
		}
		if limit != 0 && voltageDistance(peer.frame.Voltage, c.frame.Voltage) > limit {
			continue
		}
		// A second supply is read once the vehicle has stood still for a moment.
		if now.Sub(a.vehicleSince) < admissionStillTime {
			continue
		}
		// One reading decides. A pack that was read and still did not join
		// (load, fault, a pending command) is looked at again later.
		if a.sampling.pairedID == 0 || now.Sub(a.sampling.pairedAt) >= admissionRetryInterval {
			return true
		}
	}
	return false
}

func (a *packAdmission) needsPollLocked(now time.Time) bool {
	if !a.coordinated {
		return false
	}
	if a.sampling.enabled {
		return a.slots[0].monitored && a.slots[1].monitored && (a.slots[0].eligible || a.slots[1].eligible) &&
			a.decisionOpenLocked(now)
	}
	if !nonDrivingVehicle(a.vehicle) {
		return false
	}
	for _, s := range a.slots {
		if s.monitored && s.eligible && ((!s.active || s.requiresAdmission) && !s.absent || s.offPending) {
			return true
		}
	}
	return false
}
