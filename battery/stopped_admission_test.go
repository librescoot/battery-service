package battery

import (
	"sync"
	"testing"
	"time"
)

// The setting is obsolete and ignored; tests still set it to prove that.
const stoppedAdmissionSetting = "experimental.battery.dual-pack-admission-while-stopped"

func feedStoppedMotion(a *packAdmission, until time.Time) {
	for at := a.motion.at.Add(100 * time.Millisecond); !at.After(until); at = at.Add(100 * time.Millisecond) {
		a.observeMotion(true, at, at)
	}
	if a.motion.at.Before(until) {
		a.observeMotion(true, until, until)
	}
}

func readyStoppedPolicy(t *testing.T) (*packAdmission, time.Time) {
	t.Helper()
	a := samplingPolicy()
	a.setVehicleAt(VehicleStateReadyToDrive, admissionTestTime.Add(-time.Hour))
	at := admissionTestTime.Add(-100 * time.Millisecond)
	a.observeMotion(true, at, at)
	for i := 0; i < 3; i++ {
		start := admissionTestTime.Add(time.Duration(i) * admissionPollInterval)
		feedStoppedMotion(a, start)
		deliverPair(t, a, start, 52001+uint(i%2))
		feedStoppedMotion(a, start.Add(200*time.Millisecond))
	}
	return a, admissionTestTime.Add(6200 * time.Millisecond)
}

func TestStoppedAdmissionWaitsForParkedWithFreshQualifiedPair(t *testing.T) {
	a, now := readyStoppedPolicy(t)
	requireAdmission(t, a, 1, now, false)
	now = requalifyAfterParking(t, a, now)
	requireAdmission(t, a, 1, now, true)
	if !a.slots[1].held || a.slots[1].active {
		t.Fatal("ON was not reserved pending confirmation")
	}
	// A write retry cannot reuse the evidence consumed by its reservation.
	requireAdmission(t, a, 1, now, false)
	requireAdmission(t, a, 0, now, true)
	if !a.slots[1].held {
		t.Fatal("denied retry released reservation")
	}
}

func TestStoppedSettingDoesNotRequireMotionForParkedAdmission(t *testing.T) {
	a := samplingPolicy()
	for i := 0; i < 3; i++ {
		deliverPair(t, a, admissionTestTime.Add(time.Duration(i)*admissionPollInterval), 52001+uint(i%2))
	}
	requireAdmission(t, a, 1, admissionTestTime.Add(6200*time.Millisecond), true)
}

func TestDriveStateRejectsNewSupplyAcrossEvidenceFailures(t *testing.T) {
	for _, scenario := range []string{
		"qualified", "disabled", "no motion", "throttle or movement", "post-read motion missing", "motion gap", "motion stale", "future motion",
		"out of order", "no pair", "mixed round", "stale pack", "high load", "voltage mismatch",
		"no running peer", "peer OFF", "own OFF", "inactive role", "shutdown",
	} {
		t.Run(scenario, func(t *testing.T) {
			a, now := readyStoppedPolicy(t)
			switch scenario {
			case "disabled":
			case "no motion":
				a.invalidateMotion()
			case "throttle or movement":
				a.observeMotion(false, now.Add(time.Millisecond), now.Add(time.Millisecond))
				now = now.Add(time.Millisecond)
			case "post-read motion missing":
				a.motion.at = now.Add(-time.Millisecond)
			case "motion gap":
				now = now.Add(stoppedMotionMaxAge + time.Nanosecond)
				a.observeMotion(true, now, now)
			case "motion stale":
				now = now.Add(stoppedMotionMaxAge + time.Nanosecond)
			case "future motion":
				a.observeMotion(true, now.Add(time.Second), now)
			case "out of order":
				a.observeMotion(true, now.Add(-time.Millisecond), now)
			case "no pair":
				a.sampling.pairedID = 0
			case "mixed round":
				a.slots[1].observedRound++
			case "stale pack":
				a.slots[1].observed = now.Add(-admissionMaxAge - time.Nanosecond)
			case "high load":
				a.slots[0].history[0].data.Current = admissionMaxCurrent + 1
			case "voltage mismatch":
				a.slots[1].frame.Voltage += 2000
				for i := range a.slots[1].history {
					a.slots[1].history[i].data.Voltage += 2000
				}
			case "no running peer":
				a.slots[0].active = false
			case "peer OFF":
				a.offAttempt(0, now)
			case "own OFF":
				a.slots[1].offPending = true
			case "inactive role":
				a.setRole(1, false)
			case "shutdown":
				a.stopSampling()
			}
			requireAdmission(t, a, 1, now, false)
			if !a.slots[0].held {
				t.Fatal("failed admission released running supply reservation")
			}
		})
	}
}

func TestThrottlePulseDiscardsTheReading(t *testing.T) {
	a, now := readyStoppedPolicy(t)
	now = requalifyAfterParking(t, a, now)
	a.observeMotion(false, now.Add(100*time.Millisecond), now.Add(100*time.Millisecond))
	a.observeMotion(true, now.Add(200*time.Millisecond), now.Add(200*time.Millisecond))
	requireAdmission(t, a, 1, now.Add(200*time.Millisecond), false)
	at := now.Add(admissionStillTime)
	deliverPair(t, a, at, 52001)
	requireAdmission(t, a, 1, at.Add(200*time.Millisecond), true)
}

func TestStoppedAdmissionConcurrentMotionAndGate(t *testing.T) {
	a, now := readyStoppedPolicy(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 100; i++ {
			at := now.Add(time.Duration(i) * time.Millisecond)
			a.observeMotion(i%2 == 0, at, at)
		}
	}()
	for i := 1; i <= 100; i++ {
		at := now.Add(time.Duration(i) * time.Millisecond)
		a.requestOn(1, at, DefaultMaxVoltageDeltaMV)
		requireAdmission(t, a, 0, at, true)
	}
	wg.Wait()
}

func TestStoppedAdmissionDoesNotInterruptExistingSupply(t *testing.T) {
	a, now := readyStoppedPolicy(t)
	now = requalifyAfterParking(t, a, now)
	requireAdmission(t, a, 1, now, true)
	a.slots[1].active = true
	a.setVehicleAt(VehicleStateReadyToDrive, now)
	a.invalidateMotion()
	a.invalidate(0)
	a.invalidate(1)
	for i := range a.slots {
		requireAdmission(t, a, i, now.Add(time.Minute), true)
		if !a.slots[i].held || a.slots[i].offPending {
			t.Fatal("lost existing supply")
		}
	}
}

func TestECUStoppedParsesPublishedState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		values      []any
		stopped, ok bool
	}{
		{"stopped", []any{"0", "0", "off"}, true, true},
		{"moving", []any{"0", "3", "off"}, false, true},
		{"spinning", []any{"120", "0", "off"}, false, true},
		{"throttle", []any{"0", "0", "on"}, false, true},
		{"missing rpm", []any{nil, "0", "off"}, false, false},
		{"missing throttle", []any{"0", "0", nil}, false, false},
		{"bad throttle", []any{"0", "0", "maybe"}, false, false},
		{"bad speed", []any{"0", "fast", "off"}, false, false},
		{"speed overflow", []any{"0", "300", "off"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopped, ok := ecuStopped(tc.values)
			if stopped != tc.stopped || ok != tc.ok {
				t.Fatalf("got stopped=%t ok=%t", stopped, ok)
			}
		})
	}
}
