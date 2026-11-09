package battery

import (
	"fmt"
	"testing"
	"time"
)

func TestFirstQualifiedSupplyAllowsReadyToDrive(t *testing.T) {
	states := append([]VehicleState{VehicleStateReadyToDrive, VehicleStateOther, ""}, nonDriveTestStates...)
	for _, state := range states {
		for slot := 0; slot < 2; slot++ {
			t.Run(fmt.Sprintf("%s/slot%d", state, slot), func(t *testing.T) {
				a := newAdmissionTestPolicy()
				a.sampling.enabled = true
				a.setVehicleAt(state, admissionTestTime.Add(-time.Hour))
				a.tag(slot, []byte{byte(slot + 1)})
				observeWritten(a, slot, admissionFrame(55000, BMSStateIdle), admissionTestTime)
				a.observeAbsent(1-slot, admissionTestTime)
				requireAdmission(t, a, slot, admissionTestTime, nonDrivingVehicle(state) || state == VehicleStateReadyToDrive)
			})
		}
	}
}

func TestRTDFirstSupplyStillRequiresQualification(t *testing.T) {
	for _, failure := range []string{"unconfirmed", "stale", "faulted", "empty", "disabled", "shutdown", "pending-off"} {
		t.Run(failure, func(t *testing.T) {
			a := newAdmissionTestPolicy()
			a.sampling.enabled = true
			a.setVehicleAt(VehicleStateReadyToDrive, admissionTestTime.Add(-time.Hour))
			data := admissionFrame(55000, BMSStateIdle)
			a.tag(0, []byte{1})
			observeWritten(a, 0, data, admissionTestTime)
			a.observeAbsent(1, admissionTestTime)
			now := admissionTestTime
			switch failure {
			case "unconfirmed":
				a.tag(0, []byte{2})
				a.observe(0, data, now)
			case "stale":
				now = now.Add(admissionMaxAge + time.Second)
			case "faulted":
				data.FaultCode = 1
				a.observe(0, data, now)
			case "empty":
				data.Charge = 0
				data.LowSOC = true
				a.observe(0, data, now)
			case "disabled":
				a.setRole(0, false)
			case "shutdown":
				a.stopSampling()
			case "pending-off":
				a.slots[0].held = true
				a.offAttempt(0, now)
			}
			requireAdmission(t, a, 0, now, false)
		})
	}
}

func TestSingleReaderStillRequiresOwnQualifiedData(t *testing.T) {
	a := newPackAdmission(&BatteryConfiguration{Readers: []BatteryReaderConfig{
		{Index: 0, Enabled: true, Role: BatteryRoleActive},
		{Index: 1, Enabled: false, Role: BatteryRoleInactive},
	}})
	a.setVehicleAt(VehicleStateReadyToDrive, admissionTestTime.Add(-time.Hour))
	requireAdmission(t, a, 0, admissionTestTime, false)
	a.tag(0, []byte{1})
	observeWritten(a, 0, admissionFrame(55000, BMSStateIdle), admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, true)
}

func TestFreshMotionOverridesStationaryVehicleState(t *testing.T) {
	for _, state := range []VehicleState{VehicleStateParked, VehicleStateStandby} {
		for _, strict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/strict=%t", state, strict), func(t *testing.T) {
				a := samplingPolicy()
				a.conservative = strict
				a.setVehicleAt(state, admissionTestTime.Add(-time.Hour))
				for i := 0; i < 3; i++ {
					deliverPair(t, a, admissionTestTime.Add(time.Duration(i)*admissionPollInterval), 52001+uint(i%2))
				}
				now := admissionTestTime.Add(6200 * time.Millisecond)
				a.observeMotion(false, now, now)
				requireAdmission(t, a, 1, now, false)
				requireAdmission(t, a, 0, now, true)
				now = now.Add(100 * time.Millisecond)
				a.observeMotion(true, now, now)
				requireAdmission(t, a, 1, now, false)
				// Motion cannot leave a loaded pre-stop voltage pair available for reuse.
				for i := 0; i < 3; i++ {
					at := now.Add(time.Duration(i) * admissionPollInterval)
					feedStoppedMotion(a, at)
					deliverPair(t, a, at, 52001+uint(i%2))
					feedStoppedMotion(a, at.Add(200*time.Millisecond))
				}
				requireAdmission(t, a, 1, now.Add(6200*time.Millisecond), true)
			})
		}
	}
}

// Only ready-to-drive can admit a second supply outside the non-drive states,
// and only after the ECU has reported standstill; other states never do.
func TestSecondSupplyOutsideNonDriveStatesNeedsReadyToDriveStandstill(t *testing.T) {
	for _, state := range []VehicleState{VehicleStateReadyToDrive, VehicleStateOther, ""} {
		t.Run(string(state), func(t *testing.T) {
			a := samplingPolicy()
			a.setVehicleAt(state, admissionTestTime.Add(-time.Hour))
			standstillBefore(a, admissionTestTime.Add(6*time.Second), 2*admissionStillTime)
			deliverForcedPair(t, a, admissionTestTime.Add(6*time.Second), 52001)
			now := admissionTestTime.Add(6200 * time.Millisecond)
			requireAdmission(t, a, 1, now, state == VehicleStateReadyToDrive)
		})
	}
}

func TestParkingRequalifiesAfterStandstillInOtherStates(t *testing.T) {
	for _, state := range []VehicleState{VehicleStateOther, ""} {
		t.Run(string(state), func(t *testing.T) {
			a := samplingPolicy()
			a.setVehicleAt(state, admissionTestTime.Add(-time.Hour))
			deliverForcedPair(t, a, admissionTestTime, 52001)
			now := admissionTestTime.Add(time.Second)
			a.setVehicleAt(VehicleStateParked, now)
			requireAdmission(t, a, 1, now, false)
			now = requalifyAfterParking(t, a, now)
			requireAdmission(t, a, 1, now, true)
		})
	}
}

func TestLeavingDriveStateDiscardsQualification(t *testing.T) {
	for _, state := range []VehicleState{VehicleStateParked, VehicleStateStandby, VehicleStateOther, ""} {
		t.Run(string(state), func(t *testing.T) {
			a, now := readyStoppedPolicy(t)
			a.setVehicleAt(state, now)
			if a.sampling.pairedID != 0 {
				t.Fatal("drive-state qualification survived the vehicle change")
			}
			requireAdmission(t, a, 1, now, false)
			now = requalifyAfterParking(t, a, now)
			a.setVehicleAt(state, now)
			requireAdmission(t, a, 1, now, nonDrivingVehicle(state))
		})
	}
}

func TestQualifiedWindowSurvivesNonDriveStateChangesWithoutECU(t *testing.T) {
	for _, state := range nonDriveTestStates {
		t.Run(string(state), func(t *testing.T) {
			a := samplingPolicy()
			for i := 0; i < 3; i++ {
				deliverPair(t, a, admissionTestTime.Add(time.Duration(i)*admissionPollInterval), 52001+uint(i%2))
			}
			now := admissionTestTime.Add(6200 * time.Millisecond)
			id := a.sampling.pairedID
			a.setVehicleAt(state, now)
			if a.sampling.pairedID != id {
				t.Fatal("non-drive transition restarted completed qualification")
			}
			requireAdmission(t, a, 1, now, true)
		})
	}
}

func TestReaderSeatboxOffPolicyRemainsAuthoritative(t *testing.T) {
	for _, mode := range []string{"off", "manual", "aux"} {
		for slot := 0; slot < 2; slot++ {
			t.Run(fmt.Sprintf("%s/slot%d", mode, slot), func(t *testing.T) {
				r, _ := newDriveLatchReader(slot)
				r.service.admission = runningSwapPolicy()
				r.service.admission.setSeatbox(true)
				r.service.config.KeepActiveOnSeatboxOpen.Store(mode == "manual")
				r.service.config.AuxLowKeepActive.Store(mode == "aux")
				r.service.admission.setSeatbox(false)
				r.handleSeatboxLockChange(false)
				if r.ShouldSendOn() != (mode != "off") {
					t.Fatal("admission changed reader seatbox OFF policy")
				}
				r.service.admission.offAttempt(slot, admissionTestTime)
				requireAdmission(t, r.service.admission, slot, admissionTestTime, false)
			})
		}
	}
}
