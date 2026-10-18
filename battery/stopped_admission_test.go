package battery

import (
	"errors"
	"sync"
	"testing"
	"time"
)

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
	a.motion.enabled = true
	a.setVehicle(VehicleStateReadyToDrive)
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

func TestStoppedAdmissionRequiresBothSettings(t *testing.T) {
	for _, base := range []string{"false", "true"} {
		for _, stopped := range []string{"", "false", "true", "TRUE", "1", "invalid"} {
			t.Run(base+"/"+stopped, func(t *testing.T) {
				s, _ := admissionSettingService(t, map[string]string{
					"settings/experimental.battery.dual-pack-admission": base,
					"settings/" + stoppedAdmissionSetting:               stopped,
				})
				s.loadAdmissionSetting()
				got := s.admission != nil && s.admission.motion.enabled
				if got != (base == "true" && stopped == "true") {
					t.Fatalf("stopped admission=%t", got)
				}
			})
		}
	}
	s, hook := admissionSettingService(t, nil)
	s.admission = samplingPolicy()
	hook.err = errors.New("Redis unavailable")
	s.loadStoppedAdmissionSetting()
	if s.admission.motion.enabled {
		t.Fatal("read failure enabled stopped admission")
	}
}

func TestStoppedAdmissionAllowsFreshQualifiedPair(t *testing.T) {
	a, now := readyStoppedPolicy(t)
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
	a.motion.enabled = true
	for i := 0; i < 3; i++ {
		deliverPair(t, a, admissionTestTime.Add(time.Duration(i)*admissionPollInterval), 52001+uint(i%2))
	}
	requireAdmission(t, a, 1, admissionTestTime.Add(6200*time.Millisecond), true)
}

func TestStoppedAdmissionRejectsUnsafeEvidence(t *testing.T) {
	for _, scenario := range []string{
		"disabled", "no motion", "throttle or movement", "post-read motion missing", "motion gap", "motion stale", "future motion",
		"out of order", "no pair", "mixed round", "before stop", "stale pack", "high load", "voltage drift", "voltage mismatch",
		"no running peer", "peer OFF", "own OFF", "inactive role", "shutdown", "wrong state",
	} {
		t.Run(scenario, func(t *testing.T) {
			a, now := readyStoppedPolicy(t)
			switch scenario {
			case "disabled":
				a.motion.enabled = false
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
			case "before stop":
				a.motion.since = a.sampling.pairedAt.Add(time.Nanosecond)
			case "stale pack":
				a.slots[1].written = now.Add(-admissionMaxAge - time.Nanosecond)
			case "high load":
				a.slots[0].history[1].data.Current = admissionMaxCurrent + 1
			case "voltage drift":
				a.slots[1].history[1].data.Voltage += admissionVoltageSpan + 1
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
			case "wrong state":
				a.setVehicle(VehicleStateUpdating)
			}
			requireAdmission(t, a, 1, now, false)
			if !a.slots[0].held {
				t.Fatal("failed admission released running supply reservation")
			}
		})
	}
}

func TestStoppedAdmissionTimingBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		age    time.Duration
		motion bool
		want   bool
	}{
		{"motion exact", stoppedMotionMaxAge, true, true},
		{"motion expired", stoppedMotionMaxAge + time.Nanosecond, true, false},
		{"pair exact", stoppedPairMaxAge, false, true},
		{"pair expired", stoppedPairMaxAge + time.Nanosecond, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, now := readyStoppedPolicy(t)
			if tc.motion {
				now = a.motion.at.Add(tc.age)
			} else {
				now = a.sampling.pairedAt.Add(tc.age)
				feedStoppedMotion(a, now)
			}
			requireAdmission(t, a, 1, now, tc.want)
		})
	}
}

func TestStoppedAdmissionRequalifiesAfterThrottlePulse(t *testing.T) {
	a, now := readyStoppedPolicy(t)
	a.observeMotion(false, now.Add(100*time.Millisecond), now.Add(100*time.Millisecond))
	a.observeMotion(true, now.Add(200*time.Millisecond), now.Add(200*time.Millisecond))
	requireAdmission(t, a, 1, now.Add(200*time.Millisecond), false)
	for i := 3; i < 6; i++ {
		start := admissionTestTime.Add(time.Duration(i) * admissionPollInterval)
		feedStoppedMotion(a, start)
		deliverPair(t, a, start, 52001+uint(i%2))
		feedStoppedMotion(a, start.Add(200*time.Millisecond))
		if i < 5 {
			requireAdmission(t, a, 1, start.Add(200*time.Millisecond), false)
		}
	}
	requireAdmission(t, a, 1, admissionTestTime.Add(15200*time.Millisecond), true)
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
	requireAdmission(t, a, 1, now, true)
	a.slots[1].active = true
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
