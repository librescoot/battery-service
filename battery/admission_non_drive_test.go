package battery

import (
	"fmt"
	"testing"
	"time"

	"battery-service/battery/fsm"
)

func TestDriveStateGateAtReaderCommandBoundary(t *testing.T) {
	for target := 0; target < 2; target++ {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			a, now := stoppedSwapPolicy(t, false, true, "off", target, "tag")
			r := admissionTestReader(a, target)
			r.data = a.slots[target].frame
			if r.admitCommand(fsm.BMSCmdOn, now) {
				t.Fatal("reader allowed new ON in ready-to-drive")
			}
			if !r.ShouldSendOn() || !r.CheckStateCorrect() {
				t.Fatal("state denial selected OFF or mismatch recovery")
			}
			a.setVehicleAt(VehicleStateParked, now)
			if !r.admitCommand(fsm.BMSCmdOn, now) {
				t.Fatal("reader did not admit qualified pack after parking")
			}
			a.setVehicleAt(VehicleStateReadyToDrive, now)
			if r.admitCommand(fsm.BMSCmdOn, now) {
				t.Fatal("reader retried pending ON after entering ready-to-drive")
			}
			if !admissionTestReader(a, 1-target).admitCommand(fsm.BMSCmdOn, now) {
				t.Fatal("state gate blocked established peer heartbeat")
			}
		})
	}
}

func TestOnlyAdditionalSupplyWaitsForParked(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			for _, keep := range []string{"off", "manual", "aux"} {
				for target := 0; target < 2; target++ {
					for _, second := range []bool{false, true} {
						t.Run(fmt.Sprintf("strict=%t/legacy=%t/keep=%s/slot%d/second=%t", strict, legacy, keep, target, second), func(t *testing.T) {
							var a *packAdmission
							var now time.Time
							if second {
								a, now = stoppedSwapPolicy(t, strict, legacy, keep, target, "tag")
							} else {
								s, _ := admissionSettingService(t, map[string]string{
									"settings/experimental.battery.dual-pack-admission": fmt.Sprint(strict),
									"settings/" + stoppedAdmissionSetting:               fmt.Sprint(legacy),
								})
								s.batteryConfig.Readers[1].Role = BatteryRoleActive
								s.config.KeepActiveOnSeatboxOpen.Store(keep == "manual")
								s.config.AuxLowKeepActive.Store(keep == "aux")
								s.loadAdmissionSetting()
								a = s.admission
								a.setVehicle(VehicleStateReadyToDrive)
								a.tag(target, []byte{byte(target + 1)})
								observeWritten(a, target, admissionFrame(55000, BMSStateIdle), admissionTestTime)
								a.observeAbsent(1-target, admissionTestTime)
								a.observeMotion(true, admissionTestTime, admissionTestTime)
								now = admissionTestTime
							}
							requireAdmission(t, a, target, now, !second)
							if a.slots[target].held != !second {
								t.Fatal("unexpected ON reservation for first/additional supply")
							}
							a.setVehicleAt(VehicleStateParked, now)
							requireAdmission(t, a, target, now, true)
							// Only the sole supply can retry an unconfirmed ON in drive.
							a.setVehicleAt(VehicleStateReadyToDrive, now)
							requireAdmission(t, a, target, now, !second)
							if !a.slots[target].held {
								t.Fatal("drive-state rejection dropped possible power")
							}
							// Once ON is positively confirmed, this is a running supply heartbeat.
							active := a.slots[target].frame
							active.State = BMSStateActive
							now = now.Add(time.Second)
							observeWritten(a, target, active, now)
							requireAdmission(t, a, target, now, true)
						})
					}
				}
			}
		}
	}
}
