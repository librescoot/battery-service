package battery

import (
	"fmt"
	"testing"

	"battery-service/battery/fsm"
)

func TestMandatoryVoltageGuardBothInsertionOrders(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, running := range []int{0, 1} {
			for _, incomingHigher := range []bool{false, true} {
				t.Run(fmt.Sprintf("strict=%t/running=%d/incomingHigher=%t", strict, running, incomingHigher), func(t *testing.T) {
					s, _ := admissionSettingService(t, map[string]string{
						"settings/experimental.battery.dual-pack-admission": fmt.Sprint(strict),
						"settings/scooter.dual-battery":                     "true",
					})
					s.loadAdmissionSetting()
					s.loadDualBatterySetting()
					voltages := [2]uint{53000, 53000}
					if incomingHigher {
						voltages[1-running] = 57000
					} else {
						voltages[1-running] = 49000
					}
					if s.admission != nil {
						s.admission.setVehicle(VehicleStateParked)
						stablePair(s.admission, running, voltages[0], voltages[1], 0, admissionTestTime)
					}
					now := admissionTestTime.Add(admissionStableTime)
					if s.readers[1-running].admitCommand(fsm.BMSCmdOn, now) {
						t.Fatal("mismatched incoming pack received ON permission")
					}
					if !s.readers[running].admitCommand(fsm.BMSCmdOn, now) {
						t.Fatal("running supply lost its ON heartbeat")
					}
				})
			}
		}
	}
}
