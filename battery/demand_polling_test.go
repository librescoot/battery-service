package battery

import (
	"testing"
	"time"
)

func runningPair(t *testing.T) *packAdmission {
	t.Helper()
	a := runningSwapPolicy()
	a.sampling.enabled = true
	a.setVehicle(VehicleStateStandby)
	return a
}

func requireBeginRound(t *testing.T, a *packAdmission, now time.Time, want bool) uint64 {
	t.Helper()
	id, _ := a.beginRound(now)
	if (id != 0) != want {
		t.Fatalf("round opened=%t, want %t", id != 0, want)
	}
	return id
}

// Two running packs need one paired reading and then no further rounds.
func TestRunningPairIsSampledOnceThenLeftAlone(t *testing.T) {
	a := runningPair(t)
	now := admissionTestTime.Add(time.Second)
	id := requireBeginRound(t, a, now, true)
	for i := 0; i < 2; i++ {
		if a.claimRound(i, now) != id {
			t.Fatal("claim failed")
		}
		data := admissionFrame(55000, BMSStateActive)
		a.observeAcquisition(i, id, acquisition{start: now, end: now.Add(50 * time.Millisecond), complete: now.Add(100 * time.Millisecond), data: data})
	}
	if a.sampling.pairedID != id || !a.sampling.settled {
		t.Fatal("pair not accepted")
	}
	for i := 1; i <= 20; i++ {
		requireBeginRound(t, a, now.Add(time.Duration(i)*admissionPollInterval), false)
	}
}

// Anything that invalidates the pairing reopens the decision.
func TestPairingEventsReopenSampling(t *testing.T) {
	for name, event := range map[string]func(a *packAdmission, now time.Time){
		"tag":     func(a *packAdmission, now time.Time) { a.tag(1, []byte{9}) },
		"vehicle": func(a *packAdmission, now time.Time) { a.setVehicleAt(VehicleStateReadyToDrive, now) },
		"role":    func(a *packAdmission, now time.Time) { a.setRole(1, true) },
		"off":     func(a *packAdmission, now time.Time) { a.offAttempt(1, now) },
	} {
		t.Run(name, func(t *testing.T) {
			a := runningPair(t)
			now := admissionTestTime.Add(time.Second)
			a.sampling.settled = true
			requireBeginRound(t, a, now, false)
			event(a, now)
			requireBeginRound(t, a, now.Add(time.Second), true)
		})
	}
}

func TestOpenDecisionsKeepSampling(t *testing.T) {
	now := admissionTestTime.Add(time.Second)
	for name, setup := range map[string]func(a *packAdmission){
		"inactive candidate": func(a *packAdmission) { a.slots[1].active, a.slots[1].held = false, false },
		"pending OFF":        func(a *packAdmission) { a.slots[1].offPending = true },
		"needs admission":    func(a *packAdmission) { a.slots[1].requiresAdmission = true },
		"isolation":          func(a *packAdmission) { a.isolation = &isolationEpisode{survivor: 0, victim: 1} },
	} {
		t.Run(name, func(t *testing.T) {
			a := runningPair(t)
			a.sampling.settled = true
			setup(a)
			requireBeginRound(t, a, now, true)
		})
	}
}

// A second reader without a pack must not keep the first one polling.
func TestAbsentSecondPackNeedsNoSampling(t *testing.T) {
	a := runningPair(t)
	a.slots[1].held, a.slots[1].active = false, false
	a.observeAbsent(1, admissionTestTime)
	requireBeginRound(t, a, admissionTestTime.Add(time.Second), false)
}

// A heartbeat read outside any round replaces the slot's frame but does not
// reopen the decision for two running packs.
func TestHeartbeatReadDoesNotReopenSettledPair(t *testing.T) {
	a := runningPair(t)
	now := admissionTestTime.Add(time.Second)
	id := requireBeginRound(t, a, now, true)
	for i := 0; i < 2; i++ {
		a.claimRound(i, now)
		a.observeAcquisition(i, id, acquisition{start: now, end: now.Add(50 * time.Millisecond), complete: now.Add(100 * time.Millisecond), data: admissionFrame(55000, BMSStateActive)})
	}
	later := now.Add(40 * time.Second)
	a.observeAcquisition(0, 0, acquisition{start: later, end: later.Add(50 * time.Millisecond), complete: later.Add(100 * time.Millisecond), data: admissionFrame(55000, BMSStateActive)})
	requireBeginRound(t, a, later.Add(time.Second), false)
}

// An idle pack far from the running one is read once per heartbeat period; one
// within reach is read at the admission cadence.
func TestIdlePackSamplingFollowsVoltageProximity(t *testing.T) {
	for _, tc := range []struct {
		name            string
		idleMV          uint
		conservative    bool
		fast, slowAfter bool
	}{
		{"far below", 40000, false, false, true},
		{"within window", 53500, false, true, true},
		{"above, plain", 60000, false, false, true},
		{"above, experimental handover", 60000, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := runningPair(t)
			a.conservative = tc.conservative
			a.slots[1].held, a.slots[1].active = false, false
			a.slots[1].frame.Voltage = tc.idleMV
			a.slots[1].frame.State = BMSStateIdle
			a.slots[0].frame.Voltage = 55000
			a.slots[1].valid, a.slots[1].haveFrame = true, true
			now := admissionTestTime.Add(time.Second)
			requireBeginRound(t, a, now, true) // the first round is always allowed
			a.cancelRoundLocked("test")
			requireBeginRound(t, a, now.Add(admissionPollInterval), tc.fast)
			a.cancelRoundLocked("test")
			requireBeginRound(t, a, now.Add(admissionIdleInterval), tc.slowAfter)
		})
	}
}
