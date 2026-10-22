package battery

import (
	"sync"
	"testing"
	"time"
)

var admissionTestTime = time.Unix(1700000000, 0)

func newAdmissionTestPolicy() *packAdmission {
	a := newPackAdmission(&BatteryConfiguration{Readers: []BatteryReaderConfig{
		{Index: 0, Enabled: true, Role: BatteryRoleActive},
		{Index: 1, Enabled: true, Role: BatteryRoleActive},
	}})
	a.setVehicle(VehicleStateParked)
	return a
}

func admissionFrame(voltage uint, state BMSState) BMSData {
	return BMSData{Present: true, Voltage: voltage, State: state, Charge: 50}
}

func observeWritten(a *packAdmission, index int, data BMSData, now time.Time) {
	first := data
	first.Voltage--
	a.observe(index, first, now.Add(-time.Second))
	a.observe(index, data, now)
}

func requireAdmission(t *testing.T, a *packAdmission, index int, now time.Time, want bool) {
	t.Helper()
	if got, why := a.requestOn(index, now, DefaultMaxVoltageDeltaMV); got != want {
		t.Fatalf("slot %d admission = %t (%s), want %t", index, got, why, want)
	}
}

func stablePair(a *packAdmission, active int, v0, v1 uint, current int, now time.Time) {
	frames := [2]BMSData{admissionFrame(v0, BMSStateIdle), admissionFrame(v1, BMSStateIdle)}
	frames[active].State = BMSStateActive
	frames[active].Current = current
	for i, frame := range frames {
		observeWritten(a, i, frame, now)
	}
	for step := 1; step <= 2; step++ {
		for i, frame := range frames {
			frame.Voltage += uint(step % 2)
			a.observe(i, frame, now.Add(time.Duration(step)*admissionPollInterval))
		}
	}
}

func TestAdmissionSelectsInitialSupply(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		v0, v1, charge0, charge1 uint
		want                     int
	}{
		{"front higher", 55000, 51000, 80, 40, 0},
		{"rear higher", 51000, 55000, 40, 80, 1},
		{"voltage overrides charge", 55000, 51000, 40, 80, 0},
		{"charge breaks voltage tie", 53000, 53000, 40, 80, 1},
		{"slot breaks exact tie", 53000, 53000, 50, 50, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdmissionTestPolicy()
			f0, f1 := admissionFrame(tc.v0, BMSStateIdle), admissionFrame(tc.v1, BMSStateIdle)
			f0.Charge, f1.Charge = tc.charge0, tc.charge1
			observeWritten(a, 0, f0, admissionTestTime)
			observeWritten(a, 1, f1, admissionTestTime)
			requireAdmission(t, a, 1-tc.want, admissionTestTime, false)
			requireAdmission(t, a, tc.want, admissionTestTime, true)
			requireAdmission(t, a, 1-tc.want, admissionTestTime, false)
		})
	}
}

func TestAdmissionWaitsForBothReaders(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(52000, BMSStateIdle), admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, false)
	// A discovered tag with unchanged contents is not fresh evidence.
	for i := 0; i < 12; i++ {
		now := admissionTestTime.Add(time.Duration(i) * time.Second)
		a.observe(0, admissionFrame(52000+uint(i), BMSStateIdle), now)
		a.observe(1, admissionFrame(51000, BMSStateIdle), now)
		requireAdmission(t, a, 0, now, false)
	}
}

func TestAdmissionSinglePresentPack(t *testing.T) {
	for _, index := range []int{0, 1} {
		a := newAdmissionTestPolicy()
		observeWritten(a, index, admissionFrame(52000, BMSStateIdle), admissionTestTime)
		a.observeAbsent(1-index, admissionTestTime)
		requireAdmission(t, a, index, admissionTestTime, true)
	}
}

func TestAdmissionRejectsStaleAbsence(t *testing.T) {
	a := newAdmissionTestPolicy()
	a.observeAbsent(1, admissionTestTime.Add(-admissionMaxAge-time.Second))
	observeWritten(a, 0, admissionFrame(52000, BMSStateIdle), admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, false)
}

func TestAdmissionDoesNotPreferUnusablePack(t *testing.T) {
	for _, faulted := range []bool{false, true} {
		a := newAdmissionTestPolicy()
		lower, higher := admissionFrame(51000, BMSStateIdle), admissionFrame(55000, BMSStateIdle)
		if faulted {
			higher.FaultCode = 1
		} else {
			higher.LowSOC, higher.Charge = true, 0
		}
		observeWritten(a, 0, lower, admissionTestTime)
		observeWritten(a, 1, higher, admissionTestTime)
		requireAdmission(t, a, 1, admissionTestTime, false)
		requireAdmission(t, a, 0, admissionTestTime, true)
	}
}

func TestAdmissionSerializesConcurrentRequests(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		a := newAdmissionTestPolicy()
		for i := 0; i < 2; i++ {
			observeWritten(a, i, admissionFrame(53000, BMSStateIdle), admissionTestTime)
		}
		var wg sync.WaitGroup
		var got [2]bool
		for i := range got {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				got[index], _ = a.requestOn(index, admissionTestTime, DefaultMaxVoltageDeltaMV)
			}(i)
		}
		wg.Wait()
		if !got[0] || got[1] {
			t.Fatalf("concurrent decisions = %v, want [true false]", got)
		}
	}
}

func TestAdmissionRequiresStableLowLoadWindow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		v0, v1  uint
		current int
		want    bool
	}{
		{"matched", 53000, 52500, 500, true},
		{"too far apart", 55000, 51000, 0, false},
		{"hysteresis margin", 53000, 52050, 0, false},
		{"traction load", 53000, 52500, 10000, false},
		{"charging load", 53000, 52500, -2000, false},
	} {
		for _, active := range []int{0, 1} {
			t.Run(tc.name+string(rune('0'+active)), func(t *testing.T) {
				a := newAdmissionTestPolicy()
				stablePair(a, active, tc.v0, tc.v1, tc.current, admissionTestTime)
				requireAdmission(t, a, 1-active, admissionTestTime.Add(admissionStableTime), tc.want)
				handover := tc.name == "too far apart" && active == 1
				requireAdmission(t, a, active, admissionTestTime.Add(admissionStableTime), !handover)
				if handover && !a.shouldIsolate(active, admissionTestTime.Add(admissionStableTime), DefaultMaxVoltageDeltaMV) {
					t.Fatal("qualified higher-voltage handover did not request OFF")
				}
			})
		}
	}
}

func TestAdmissionNotOnOnePassingSample(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(53000, BMSStateActive), admissionTestTime)
	observeWritten(a, 1, admissionFrame(52500, BMSStateIdle), admissionTestTime)
	requireAdmission(t, a, 1, admissionTestTime, false)
	// Reusing observations cannot manufacture a stable window.
	requireAdmission(t, a, 1, admissionTestTime.Add(admissionStableTime), false)
}

func TestAdmissionRejectsSagAndStaleData(t *testing.T) {
	for _, kind := range []string{"sag", "stale", "skew", "read failure", "unchanged tag"} {
		t.Run(kind, func(t *testing.T) {
			a := newAdmissionTestPolicy()
			stablePair(a, 0, 53000, 52500, 0, admissionTestTime)
			now := admissionTestTime.Add(admissionStableTime)
			switch kind {
			case "sag":
				a.observe(0, admissionFrame(52800, BMSStateActive), now)
			case "stale":
				now = now.Add(admissionMaxAge + time.Second)
			case "skew":
				now = now.Add(admissionMaxSkew + time.Second)
				a.observe(1, admissionFrame(52501, BMSStateIdle), now)
			case "read failure":
				a.invalidate(0)
			case "unchanged tag":
				for i := 1; i < 6; i++ {
					now = now.Add(admissionPollInterval)
					a.observe(0, admissionFrame(53000, BMSStateActive), now)
					a.observe(1, admissionFrame(52500, BMSStateIdle), now)
				}
			}
			requireAdmission(t, a, 1, now, false)
			requireAdmission(t, a, 0, now, true)
		})
	}
}

func TestAdmissionRetriesAfterConvergence(t *testing.T) {
	a := newAdmissionTestPolicy()
	stablePair(a, 0, 55000, 51000, 0, admissionTestTime)
	requireAdmission(t, a, 1, admissionTestTime.Add(admissionStableTime), false)
	// No insertion, role change or service restart is necessary.
	now := admissionTestTime.Add(time.Minute)
	stablePair(a, 0, 51500, 51000, 0, now)
	requireAdmission(t, a, 1, now.Add(admissionStableTime), true)
}

func TestAdmissionFaultedSupplyDoesNotAcceptAnotherPack(t *testing.T) {
	a := newAdmissionTestPolicy()
	stablePair(a, 0, 53000, 52500, 0, admissionTestTime)
	faulted := admissionFrame(53000, BMSStateActive)
	faulted.FaultCode = 1
	now := admissionTestTime.Add(admissionStableTime)
	a.observe(0, faulted, now)
	requireAdmission(t, a, 1, now, false)
	requireAdmission(t, a, 0, now, true)
}

func TestAdmissionPendingOnDoesNotStartWhileDriving(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(53000, BMSStateIdle), admissionTestTime)
	a.observeAbsent(1, admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, true)
	a.setVehicle(VehicleStateReadyToDrive)
	requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), false)
	a.observe(0, admissionFrame(53000, BMSStateActive), admissionTestTime.Add(time.Second))
	requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), true)
}

func TestAdmissionVehicleBoundaries(t *testing.T) {
	for _, state := range []VehicleState{VehicleStateStandby, VehicleStateReadyToDrive, VehicleStateUpdating, VehicleStateShuttingDown, VehicleStateOther, ""} {
		a := newAdmissionTestPolicy()
		a.setVehicle(state)
		stablePair(a, 0, 53000, 52500, 0, admissionTestTime)
		requireAdmission(t, a, 0, admissionTestTime.Add(admissionStableTime), true)
		requireAdmission(t, a, 1, admissionTestTime.Add(admissionStableTime), false)
	}
	a := newAdmissionTestPolicy()
	a.setVehicle(VehicleStateReadyToDrive)
	stablePair(a, 0, 53000, 52500, 0, admissionTestTime)
	now := admissionTestTime.Add(admissionStableTime)
	a.setVehicle(VehicleStateParked)
	requireAdmission(t, a, 1, now, false)
	stablePair(a, 0, 53000, 52500, 0, now.Add(time.Second))
	requireAdmission(t, a, 1, now.Add(time.Second+admissionStableTime), true)
}

func TestAdmissionPreservesBothRunningPacks(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(55000, BMSStateActive), admissionTestTime)
	observeWritten(a, 1, admissionFrame(51000, BMSStateActive), admissionTestTime)
	a.setVehicle(VehicleStateReadyToDrive)
	for _, index := range []int{0, 1} {
		a.invalidate(index)
		requireAdmission(t, a, index, admissionTestTime.Add(time.Hour), true)
	}
}

func TestAdmissionUnexpectedDeactivationRequiresNewAdmission(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(55000, BMSStateActive), admissionTestTime)
	observeWritten(a, 1, admissionFrame(51000, BMSStateActive), admissionTestTime)
	a.setVehicle(VehicleStateReadyToDrive)
	now := admissionTestTime.Add(time.Second)
	a.observe(1, admissionFrame(51000, BMSStateIdle), now)
	requireAdmission(t, a, 1, now, false)
	requireAdmission(t, a, 0, now, true)
	a.setVehicle(VehicleStateParked)
	stablePair(a, 0, 55000, 51000, 0, now)
	requireAdmission(t, a, 1, now.Add(admissionStableTime), false)
}

func TestAdmissionOffOnCycleRechecksAndReselects(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(51000, BMSStateActive), admissionTestTime)
	observeWritten(a, 1, admissionFrame(55000, BMSStateIdle), admissionTestTime)
	// Never replace the lower-voltage running supply with an incompatible pack.
	requireAdmission(t, a, 1, admissionTestTime, false)
	now := admissionTestTime.Add(time.Second)
	a.offAttempt(0, now)
	requireAdmission(t, a, 0, now, false)
	a.observe(0, admissionFrame(51000, BMSStateIdle), now.Add(time.Second))
	requireAdmission(t, a, 0, now.Add(time.Second), false)
	requireAdmission(t, a, 1, now.Add(time.Second), true)
}

func TestAdmissionFailedOnWriteDoesNotReleaseReservation(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(53000, BMSStateIdle), admissionTestTime)
	observeWritten(a, 1, admissionFrame(52500, BMSStateIdle), admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, true)
	a.invalidate(0)
	a.observeAbsent(0, admissionTestTime)
	requireAdmission(t, a, 1, admissionTestTime, false)
	// A subsequent positive inactive frame can release the reservation.
	now := admissionTestTime.Add(admissionOnSettle + time.Second)
	observeWritten(a, 0, admissionFrame(52000, BMSStateIdle), now)
	observeWritten(a, 1, admissionFrame(52500, BMSStateIdle), now)
	requireAdmission(t, a, 1, now, true)
}

func TestAdmissionNewTagCannotInheritPermission(t *testing.T) {
	a := newAdmissionTestPolicy()
	a.tag(0, []byte{1})
	observeWritten(a, 0, admissionFrame(53000, BMSStateActive), admissionTestTime)
	a.tag(0, []byte{2})
	requireAdmission(t, a, 0, admissionTestTime, false)
}

func TestAdmissionDisablingDeltaDoesNotBypassOtherGates(t *testing.T) {
	a := newAdmissionTestPolicy()
	if ok, _ := a.requestOn(0, admissionTestTime, 0); ok {
		t.Fatal("missing data permitted ON with delta protection disabled")
	}
	stablePair(a, 0, 55000, 51000, 0, admissionTestTime)
	if ok, why := a.requestOn(1, admissionTestTime.Add(admissionStableTime), 0); !ok {
		t.Fatalf("disabled voltage limit still blocked stable parked admission: %s", why)
	}
}

func TestAdmissionRoleChangesRetainProtection(t *testing.T) {
	a := newAdmissionTestPolicy()
	stablePair(a, 1, 51000, 55000, 0, admissionTestTime)
	a.setRole(1, false)
	requireAdmission(t, a, 1, admissionTestTime.Add(admissionStableTime), false)
	requireAdmission(t, a, 0, admissionTestTime.Add(admissionStableTime), false)
	a.setRole(1, true)
	requireAdmission(t, a, 1, admissionTestTime.Add(admissionStableTime), true)
}

func TestAdmissionSingleModeUsesObservedPeerState(t *testing.T) {
	a := newPackAdmission(&BatteryConfiguration{Readers: []BatteryReaderConfig{
		{Index: 0, Enabled: true, Role: BatteryRoleActive},
		{Index: 1, Enabled: true, Role: BatteryRoleInactive},
	}})
	a.setVehicle(VehicleStateParked)
	observeWritten(a, 0, admissionFrame(53000, BMSStateIdle), admissionTestTime)
	a.observeAbsent(1, admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, true)
	a.setRole(1, true)
	requireAdmission(t, a, 0, admissionTestTime, true)
	requireAdmission(t, a, 1, admissionTestTime, false)
}

func TestAdmissionEnableDuringSinglePackOn(t *testing.T) {
	a := newPackAdmission(&BatteryConfiguration{Readers: []BatteryReaderConfig{
		{Index: 0, Enabled: true, Role: BatteryRoleActive},
		{Index: 1, Enabled: true, Role: BatteryRoleInactive},
	}})
	a.setVehicle(VehicleStateParked)
	observeWritten(a, 0, admissionFrame(51000, BMSStateIdle), admissionTestTime)
	observeWritten(a, 1, admissionFrame(55000, BMSStateIdle), admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, true)
	a.setRole(1, true)
	// The front pack may already be energizing the bus even though its
	// latest status still says idle. Selection must not start the rear one.
	requireAdmission(t, a, 1, admissionTestTime, false)
}

func TestAdmissionSinglePresentPackDoesNotAccelerateStandbyPolling(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(53000, BMSStateActive), admissionTestTime)
	a.observeAbsent(1, admissionTestTime)
	for _, state := range []VehicleState{VehicleStateStandby, VehicleStateParked} {
		a.setVehicle(state)
		if a.needsPoll() {
			t.Fatalf("single active pack with an absent peer requests polling in %s", state)
		}
	}
}

func TestAdmissionPolling(t *testing.T) {
	a := newAdmissionTestPolicy()
	if !a.needsPoll() {
		t.Fatal("initial selection needs polling")
	}
	a.setVehicle(VehicleStateReadyToDrive)
	if a.needsPoll() {
		t.Fatal("driving must not accelerate admission polling")
	}
	a.setVehicle(VehicleStateParked)
	for i := 0; i < 2; i++ {
		observeWritten(a, i, admissionFrame(53000, BMSStateActive), admissionTestTime)
	}
	if a.needsPoll() {
		t.Fatal("two confirmed active packs need only normal heartbeats")
	}
	a.observe(1, admissionFrame(53000, BMSStateIdle), admissionTestTime.Add(time.Second))
	if !a.needsPoll() {
		t.Fatal("an inactive pack needs parked retries")
	}
}
