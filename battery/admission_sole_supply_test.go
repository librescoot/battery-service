package battery

import (
	"fmt"
	"testing"

	"battery-service/battery/fsm"
)

func TestFirstSupplyAtReaderBoundaryInRTD(t *testing.T) {
	for slot := 0; slot < 2; slot++ {
		for _, moving := range []bool{false, true} {
			t.Run(fmt.Sprintf("slot%d/moving=%t", slot, moving), func(t *testing.T) {
				a := newAdmissionTestPolicy()
				a.motion.enabled = true
				a.setVehicle(VehicleStateReadyToDrive)
				a.tag(slot, []byte{byte(slot + 1)})
				data := admissionFrame(55000, BMSStateIdle)
				observeWritten(a, slot, data, admissionTestTime)
				a.observeAbsent(1-slot, admissionTestTime)
				a.observeMotion(!moving, admissionTestTime, admissionTestTime)
				r := admissionTestReader(a, slot)
				r.data = data
				for attempt := 0; attempt < 2; attempt++ {
					if !r.admitCommand(fsm.BMSCmdOn, admissionTestTime) {
						t.Fatal("sole qualified supply blocked in ready-to-drive")
					}
				}
			})
		}
	}
}

func TestRTDSelectsExactlyOneOfTwoInactivePacks(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for higher := 0; higher < 2; higher++ {
			t.Run(fmt.Sprintf("strict=%t/higher=%d", strict, higher), func(t *testing.T) {
				a := newAdmissionTestPolicy()
				a.conservative = strict
				a.setVehicle(VehicleStateReadyToDrive)
				for slot := 0; slot < 2; slot++ {
					voltage := uint(54000)
					if slot == higher {
						voltage = 55000
					}
					a.tag(slot, []byte{byte(slot + 1)})
					observeWritten(a, slot, admissionFrame(voltage, BMSStateIdle), admissionTestTime)
				}
				selected := 0
				if strict {
					selected = higher
				}
				requireAdmission(t, a, selected, admissionTestTime, true)
				requireAdmission(t, a, 1-selected, admissionTestTime, false)
				if a.slots[1-selected].held {
					t.Fatal("pending first ON permitted a second reservation")
				}
			})
		}
	}
}

func TestRTDUncertainPeerIsNotAnAbsentSupply(t *testing.T) {
	for slot := 0; slot < 2; slot++ {
		for _, state := range []string{"active", "pending-on", "pending-off", "rf-absent"} {
			t.Run(fmt.Sprintf("slot%d/%s", slot, state), func(t *testing.T) {
				a, now := stoppedSwapPolicy(t, false, true, "aux", slot, "tag")
				peer := 1 - slot
				switch state {
				case "pending-on":
					a.slots[peer].active = false
				case "pending-off":
					a.offAttempt(peer, now)
				case "rf-absent":
					a.observeAbsent(peer, now)
				}
				requireAdmission(t, a, slot, now, false)
				if !a.slots[peer].held {
					t.Fatal("lost possible peer supply")
				}
			})
		}
	}
}
