package battery

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"battery-service/battery/fsm"
)

func safetyPair(t *testing.T, a *packAdmission, now time.Time, frames [2]BMSData) time.Time {
	t.Helper()
	id, _ := a.beginRound(now)
	if id == 0 {
		t.Fatal("no sampling round")
	}
	for i, frame := range frames {
		if a.claimRound(i, now) != id {
			t.Fatal("round claim failed")
		}
		a.observeAcquisition(i, id, acquisition{start: now, end: now.Add(100 * time.Millisecond), complete: now.Add(200 * time.Millisecond), data: frame})
	}
	return now.Add(200 * time.Millisecond)
}

func TestBaselineRequiresFreshPairedVoltageAtExactLimit(t *testing.T) {
	for _, delta := range []uint{999, 1000, 1001} {
		for _, running := range []int{0, 1} {
			a := newAdmissionTestPolicy()
			a.conservative, a.sampling.enabled = false, true
			frames := [2]BMSData{admissionFrame(53000, BMSStateIdle), admissionFrame(53000, BMSStateIdle)}
			frames[running].State = BMSStateActive
			frames[1-running].Voltage += delta
			for i, f := range frames {
				observeWritten(a, i, f, admissionTestTime.Add(-time.Second))
			}
			requireAdmission(t, a, 1-running, admissionTestTime, false)
			now := safetyPair(t, a, admissionTestTime, frames)
			requireAdmission(t, a, 1-running, now, delta <= 1000)
			requireAdmission(t, a, running, now, true)
		}
	}
}

func TestBaselineSerializesSimultaneousOn(t *testing.T) {
	for n := 0; n < 100; n++ {
		a := newAdmissionTestPolicy()
		a.conservative = false
		for i := range a.slots {
			observeWritten(a, i, admissionFrame(53000, BMSStateIdle), admissionTestTime)
		}
		var got [2]bool
		var wg sync.WaitGroup
		for i := range got {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				got[i], _ = a.requestOn(i, admissionTestTime, 1000)
			}(i)
		}
		wg.Wait()
		if !got[0] || got[1] {
			t.Fatalf("unsafe simultaneous decisions: %v", got)
		}
	}
}

func TestSingleModeCannotEraseOffOrEligibility(t *testing.T) {
	a := newPackAdmission(&BatteryConfiguration{Readers: []BatteryReaderConfig{
		{Index: 0, Enabled: true, Role: BatteryRoleActive},
		{Index: 1, Enabled: true, Role: BatteryRoleInactive},
	}})
	a.setVehicle(VehicleStateParked)
	observeWritten(a, 0, admissionFrame(53000, BMSStateIdle), admissionTestTime)
	a.observeAbsent(1, admissionTestTime)
	requireAdmission(t, a, 1, admissionTestTime, false)
	requireAdmission(t, a, 0, admissionTestTime, true)
	a.offAttempt(0, admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), false)
	if !a.slots[0].held || !a.slots[0].offPending {
		t.Fatal("single mode discarded an unresolved OFF")
	}
}

func TestTagChangePreservesUnresolvedPower(t *testing.T) {
	for _, off := range []bool{false, true} {
		a := newAdmissionTestPolicy()
		a.tag(0, []byte{1})
		observeWritten(a, 0, admissionFrame(53000, BMSStateIdle), admissionTestTime)
		observeWritten(a, 1, admissionFrame(52000, BMSStateIdle), admissionTestTime)
		requireAdmission(t, a, 0, admissionTestTime, true)
		if off {
			a.offAttempt(0, admissionTestTime)
		}
		a.tag(0, []byte{2})
		if !a.slots[0].held || a.slots[0].active || a.slots[0].offPending != off {
			t.Fatal("tag change erased uncertain power or inherited confirmation")
		}
		requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), false)
		requireAdmission(t, a, 1, admissionTestTime.Add(time.Second), false)
	}
}

func TestIsolationLatchesOneVictimAndRequiresOffProof(t *testing.T) {
	for _, survivor := range []int{0, 1} {
		a := newAdmissionTestPolicy()
		frames := [2]BMSData{admissionFrame(51000, BMSStateActive), admissionFrame(51000, BMSStateActive)}
		frames[survivor].Voltage = 55000
		for i, frame := range frames {
			observeWritten(a, i, frame, admissionTestTime)
		}
		victim := 1 - survivor
		if !a.shouldIsolate(victim, admissionTestTime, 1000) || a.shouldIsolate(survivor, admissionTestTime, 1000) {
			t.Fatal("did not select exactly the lower-voltage running pack")
		}
		// Voltage ordering changes during interleaved callbacks, but the
		// selected survivor must never become the second OFF target.
		now := admissionTestTime.Add(time.Second)
		frames[victim].Voltage, frames[survivor].Voltage = 56000, 52000
		for i, frame := range frames {
			a.observe(i, frame, now)
		}
		if a.shouldIsolate(survivor, now, 1000) || !a.shouldIsolate(victim, now, 1000) {
			t.Fatal("isolation reversed after voltage ordering changed")
		}
		requireAdmission(t, a, survivor, now, true)
		requireAdmission(t, a, victim, now, false)
		if !a.isolationOffAttempt(victim, now) {
			t.Fatal("OFF refused despite confirmed survivor")
		}
		// A failed write or a repeated ACTIVE response is not OFF proof.
		a.observe(victim, frames[victim], now.Add(time.Second))
		requireAdmission(t, a, victim, now.Add(time.Second), false)
		if !a.slots[victim].offPending || !a.slots[victim].held {
			t.Fatal("unconfirmed OFF released the reservation")
		}
		frames[victim].State = BMSStateIdle
		a.observe(victim, frames[victim], now.Add(2*time.Second))
		if a.isolation != nil || a.slots[victim].held || a.slots[victim].offPending {
			t.Fatal("positive post-command OFF did not finish isolation")
		}
		requireAdmission(t, a, victim, now.Add(2*time.Second), false)
	}
}

func TestIsolationNeedsFreshPairAndSafeVehicle(t *testing.T) {
	for _, state := range []VehicleState{VehicleStateParked, VehicleStateStandby, VehicleStateReadyToDrive} {
		a := newAdmissionTestPolicy()
		a.setVehicle(state)
		a.sampling.enabled = true
		frames := [2]BMSData{admissionFrame(55000, BMSStateActive), admissionFrame(51000, BMSStateActive)}
		for i, frame := range frames {
			observeWritten(a, i, frame, admissionTestTime.Add(-time.Second))
		}
		if a.shouldIsolate(1, admissionTestTime, 1000) {
			t.Fatal("unpaired data triggered isolation")
		}
		now := safetyPair(t, a, admissionTestTime, frames)
		if got := a.shouldIsolate(1, now, 1000); got != (state != VehicleStateReadyToDrive) {
			t.Fatalf("isolation in %s = %t", state, got)
		}
	}
}

func TestIsolationRechecksSurvivorBeforeEveryOffAttempt(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		a := newAdmissionTestPolicy()
		for i, voltage := range []uint{55000, 51000} {
			observeWritten(a, i, admissionFrame(voltage, BMSStateActive), admissionTestTime)
		}
		a.shouldIsolate(1, admissionTestTime, 1000)
		if attempted && !a.isolationOffAttempt(1, admissionTestTime) {
			t.Fatal("initial OFF denied")
		}
		a.invalidate(0)
		if a.isolationOffAttempt(1, admissionTestTime.Add(time.Second)) {
			t.Fatal("OFF allowed after survivor evidence was lost")
		}
		requireAdmission(t, a, 1, admissionTestTime.Add(time.Second), !attempted)
		requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), true)
	}
}

func TestIsolationReaderSelectsOffWithoutChangingEnabled(t *testing.T) {
	s, _ := admissionSettingService(t, nil)
	s.admission = newAdmissionTestPolicy()
	now := time.Now()
	for i, voltage := range []uint{51000, 55000} {
		observeWritten(s.admission, i, admissionFrame(voltage, BMSStateActive), now)
		s.readers[i].enabled, s.readers[i].role = true, BatteryRoleActive
		s.readers[i].data = admissionFrame(voltage, BMSStateActive)
	}
	if s.readers[0].ShouldSendOn() || !s.readers[1].ShouldSendOn() || !s.readers[0].enabled {
		t.Fatal("forced OFF selection changed the user enable flag or selected the survivor")
	}
	if !s.readers[0].admitCommandPolicy(fsm.BMSCmdOff, now, true) {
		t.Fatal("reader did not issue the isolation OFF")
	}
	s.admission.invalidate(1)
	if s.readers[0].admitCommandPolicy(fsm.BMSCmdOff, now.Add(time.Second), true) {
		t.Fatal("reader retry bypassed survivor confirmation")
	}
}

func TestAdmissionDiagnosticsAreIndependentAndReadOnly(t *testing.T) {
	a := samplingPolicy()
	for i := 0; i < 3; i++ {
		deliverPair(t, a, admissionTestTime.Add(time.Duration(i)*admissionPollInterval), 52001+uint(i%2))
	}
	now := admissionTestTime.Add(6*time.Second + 200*time.Millisecond)
	before := a.cloneLocked()
	for i := 0; i < 10; i++ {
		text, key := a.diagnostics(now, 1000)
		for _, want := range []string{"baseline_new_on=true", "strict_new_on=true", "reason=\"maintaining supply\"", "changed_age=", "off_pending=", "paired_round="} {
			if !strings.Contains(text, want) {
				t.Fatalf("diagnostic missing %q: %s", want, text)
			}
		}
		if key == "" {
			t.Fatal("missing diagnostic decision key")
		}
	}
	if !reflect.DeepEqual(before, a.cloneLocked()) {
		t.Fatal("diagnostics mutated live reservations, histories or sampling")
	}
	requireAdmission(t, a, 1, now, true)
}
