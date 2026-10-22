package battery

import (
	"testing"
	"time"
)

func TestHandoverKeepaliveRequiresNewOffAndSettling(t *testing.T) {
	for _, old := range []int{0, 1} {
		a := newAdmissionTestPolicy()
		voltages := [2]uint{55000, 55000}
		voltages[old] = 51000
		stablePair(a, old, voltages[0], voltages[1], 0, admissionTestTime)
		now := admissionTestTime.Add(admissionStableTime)
		if !a.shouldIsolate(old, now, 1000) || !a.isolationOffAttempt(old, now) {
			t.Fatal("handover failed to start")
		}
		a.setVehicle(VehicleStateReadyToDrive)
		now = now.Add(time.Second)
		a.observe(old, admissionFrame(51001, BMSStateActive), now)
		now = now.Add(100 * time.Millisecond)
		requireAdmission(t, a, old, now, true)
		lastOn := now
		now = now.Add(time.Second)
		a.observe(old, admissionFrame(51002, BMSStateIdle), now)
		if !a.slots[old].held || !a.slots[old].offPending || a.isolation.offConfirmed {
			t.Fatal("pre-keepalive OFF accepted as current OFF proof")
		}
		requireAdmission(t, a, 1-old, now, false)
		if a.isolationOffAttempt(old, now) {
			t.Fatal("corrective OFF while driving")
		}
		a.setVehicle(VehicleStateParked)
		now = now.Add(time.Second)
		if !a.isolationOffAttempt(old, now) || !a.slots[old].offAt.After(lastOn) {
			t.Fatal("new parked OFF was not ordered after the keepalive")
		}
		off := now
		a.observe(old, admissionFrame(51003, BMSStateIdle), off.Add(BMSTimeCmd-time.Millisecond))
		if !a.slots[old].held || !a.slots[old].offPending {
			t.Fatal("OFF proof accepted before command settling")
		}
		now = off.Add(BMSTimeCmd + time.Millisecond)
		a.observe(old, admissionFrame(51004, BMSStateIdle), now)
		if a.slots[old].held || a.slots[old].offPending || !a.isolation.offConfirmed {
			t.Fatal("settled new OFF did not confirm")
		}
		requireAdmission(t, a, 1-old, now, true)
	}
}
