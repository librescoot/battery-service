package battery

import (
	"fmt"
	"testing"
	"time"
)

func TestRunningSupplySurvivesRapidVehicleReentry(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, keep := range []bool{false, true} {
			for _, both := range []bool{false, true} {
				for slot := 0; slot < 2; slot++ {
					for _, event := range []string{"drive-seatbox", "drive-rf-return", "parked-seatbox"} {
						t.Run(fmt.Sprintf("strict=%t/keep=%t/both=%t/slot%d/%s", strict, keep, both, slot, event), func(t *testing.T) {
							s, _ := admissionSettingService(t, map[string]string{"settings/experimental.battery.dual-pack-admission": fmt.Sprint(strict)})
							s.batteryConfig.Readers[1].Role = BatteryRoleActive
							s.config.KeepActiveOnSeatboxOpen.Store(keep)
							s.loadAdmissionSetting()
							a := s.admission
							a.setVehicle(VehicleStateParked)
							a.setSeatbox(true)
							for i := range a.slots {
								if !both && i != slot {
									a.observeAbsent(i, admissionTestTime)
									continue
								}
								a.tag(i, []byte{byte(i + 1)})
								data := admissionFrame(55000, BMSStateActive)
								data.SerialNumber = fmt.Sprintf("original-%d", i)
								data.Current = 2500
								observeWritten(a, i, data, admissionTestTime)
							}
							now := admissionTestTime.Add(time.Second)
							a.setVehicle(VehicleStateReadyToDrive)
							switch event {
							case "drive-seatbox":
								a.setSeatbox(false)
								a.setSeatbox(true)
								a.setVehicle(VehicleStateParked)
							case "drive-rf-return":
								data := a.slots[slot].frame
								a.observeAbsent(slot, now)
								a.tag(slot, []byte{byte(slot + 1)})
								observeWritten(a, slot, data, now.Add(time.Second))
								now = now.Add(time.Second)
								a.setVehicle(VehicleStateParked)
							case "parked-seatbox":
								a.setVehicle(VehicleStateParked)
								a.setSeatbox(false)
								a.setSeatbox(true)
							}
							// Vehicle's established drive-entry contract accepts present ACTIVE
							// packs. A rapid stand cycle must not then withdraw their heartbeats.
							a.setVehicle(VehicleStateReadyToDrive)
							requireAdmission(t, a, slot, now, true)
							if both {
								requireAdmission(t, a, 1-slot, now, true)
							}
							a.tag(slot, []byte{9})
							requireAdmission(t, a, slot, now, false)
							replacement := admissionFrame(50000, BMSStateActive)
							replacement.SerialNumber = "replacement"
							observeWritten(a, slot, replacement, now.Add(time.Second))
							requireAdmission(t, a, slot, now.Add(time.Second), !both)
							a.setVehicleAt(VehicleStateParked, now.Add(time.Second))
							requireAdmission(t, a, slot, now.Add(time.Second), !both)
						})
					}
				}
			}
		}
	}
}
