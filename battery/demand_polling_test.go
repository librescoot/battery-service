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
	if a.sampling.pairedID != id {
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
			a.sampling.pairedID, a.sampling.pairedAt = 7, now
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
			a.sampling.pairedID, a.sampling.pairedAt = 7, now
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
