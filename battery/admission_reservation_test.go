package battery

import (
	"testing"
	"time"
)

func TestSingleRoleDoesNotIgnoreRunningPeer(t *testing.T) {
	for _, pendingOff := range []bool{false, true} {
		a := newPackAdmission(&BatteryConfiguration{Readers: []BatteryReaderConfig{
			{Index: 0, Enabled: true, Role: BatteryRoleActive},
			{Index: 1, Enabled: true, Role: BatteryRoleInactive},
		}})
		a.conservative = false
		a.setVehicle(VehicleStateParked)
		observeWritten(a, 0, admissionFrame(51000, BMSStateIdle), admissionTestTime)
		observeWritten(a, 1, admissionFrame(55000, BMSStateActive), admissionTestTime)
		if pendingOff {
			a.offAttempt(1, admissionTestTime)
		}
		requireAdmission(t, a, 0, admissionTestTime, false)
		a.offAttempt(1, admissionTestTime)
		a.observe(1, admissionFrame(55000, BMSStateIdle), admissionTestTime.Add(time.Second))
		requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), true)
	}
}

func TestTagChangeDoesNotShortenUnconfirmedOnSettle(t *testing.T) {
	a := newAdmissionTestPolicy()
	a.tag(0, []byte{1})
	observeWritten(a, 0, admissionFrame(55000, BMSStateIdle), admissionTestTime)
	observeWritten(a, 1, admissionFrame(51000, BMSStateIdle), admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, true)
	a.tag(0, []byte{2})
	a.observe(0, admissionFrame(55000, BMSStateIdle), admissionTestTime.Add(time.Second))
	a.observe(0, admissionFrame(55001, BMSStateIdle), admissionTestTime.Add(2*time.Second))
	if !a.slots[0].held || !a.slots[0].onAt.Equal(admissionTestTime) {
		t.Fatal("UID change shortened the uncertain ON settling period")
	}
	requireAdmission(t, a, 0, admissionTestTime.Add(2*time.Second), false)
	requireAdmission(t, a, 1, admissionTestTime.Add(2*time.Second), false)
}

func TestPendingSecondOnRechecksPeerVoltage(t *testing.T) {
	for _, conservative := range []bool{false, true} {
		a := newAdmissionTestPolicy()
		a.conservative = conservative
		stablePair(a, 0, 53000, 52500, 0, admissionTestTime)
		now := admissionTestTime.Add(admissionStableTime)
		requireAdmission(t, a, 1, now, true)
		a.observe(0, admissionFrame(55000, BMSStateActive), now.Add(time.Second))
		a.observe(1, admissionFrame(52500, BMSStateIdle), now.Add(time.Second))
		requireAdmission(t, a, 1, now.Add(time.Second), false)
		requireAdmission(t, a, 0, now.Add(time.Second), true)
		if !a.slots[1].held {
			t.Fatal("voltage rejection erased a possibly successful ON")
		}
	}
}

func TestPausedHandoverRequiresNewOffAfterKeepalive(t *testing.T) {
	a := newAdmissionTestPolicy()
	stablePair(a, 0, 51000, 55000, 0, admissionTestTime)
	now := admissionTestTime.Add(admissionStableTime)
	if !a.shouldIsolate(0, now, 1000) || !a.isolationOffAttempt(0, now) {
		t.Fatal("handover did not begin")
	}
	a.setVehicle(VehicleStateReadyToDrive)
	requireAdmission(t, a, 0, now.Add(100*time.Millisecond), false)
	a.observe(0, admissionFrame(51001, BMSStateActive), now.Add(time.Second))
	a.observe(1, admissionFrame(55001, BMSStateIdle), now.Add(time.Second))
	requireAdmission(t, a, 0, now.Add(2*time.Second), true)
	a.observe(0, admissionFrame(51001, BMSStateIdle), now.Add(3*time.Second))
	if !a.slots[0].held || !a.slots[0].offPending || a.isolation.offConfirmed {
		t.Fatal("an old OFF confirmed after a newer keepalive")
	}
	requireAdmission(t, a, 1, now.Add(3*time.Second), false)
	a.setVehicle(VehicleStateParked)
	if !a.isolationOffAttempt(0, now.Add(4*time.Second)) {
		t.Fatal("new parked OFF was denied")
	}
	a.observe(0, admissionFrame(51002, BMSStateIdle), now.Add(5*time.Second))
	requireAdmission(t, a, 1, now.Add(5*time.Second), true)
}

func TestIsolationOffRetryWaitsWhileDriving(t *testing.T) {
	a := newAdmissionTestPolicy()
	for i, voltage := range []uint{55000, 51000} {
		observeWritten(a, i, admissionFrame(voltage, BMSStateActive), admissionTestTime)
	}
	if !a.shouldIsolate(1, admissionTestTime, 1000) || !a.isolationOffAttempt(1, admissionTestTime) {
		t.Fatal("parked isolation did not start")
	}
	// A failed or ambiguous write cannot justify another OFF while moving,
	// nor may its unresolved reservation be reset to permit another ON.
	a.setVehicle(VehicleStateReadyToDrive)
	if a.isolationOffAttempt(1, admissionTestTime.Add(time.Second)) {
		t.Fatal("OFF retry authorized while ready-to-drive")
	}
	requireAdmission(t, a, 1, admissionTestTime.Add(time.Second), false)
	requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), true)
	if !a.slots[1].held || !a.slots[1].offPending {
		t.Fatal("driving erased the unresolved OFF")
	}
	a.setVehicle(VehicleStateParked)
	if !a.isolationOffAttempt(1, admissionTestTime.Add(2*time.Second)) {
		t.Fatal("parked OFF retry did not resume")
	}
}
