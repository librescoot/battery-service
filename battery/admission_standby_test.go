package battery

import (
	"fmt"
	"testing"
	"time"
)

var nonDriveTestStates = []VehicleState{
	VehicleStateStandby, VehicleStateParked, VehicleStateHopOn, VehicleStateHopOnLearning,
	VehicleStateWaitingSeatbox, VehicleStateShuttingDown, VehicleStateUpdating,
	VehicleStateWaitingHibernation, VehicleStateWaitingHibernationAdvanced,
	VehicleStateWaitingHibernationSeatbox, VehicleStateWaitingHibernationConfirm,
}

func TestNonDriveCompatiblePacksJoinInEitherOrder(t *testing.T) {
	for _, state := range nonDriveTestStates {
		for _, mode := range []string{"dual", "experimental"} {
			for _, active := range []int{0, 1} {
				t.Run(fmt.Sprintf("%s/%s/active%d", state, mode, active), func(t *testing.T) {
					s := newAdmissionSimulation(t, mode)
					s.a.setVehicle(state)
					s.insert(active, 53000, 80, BMSStateActive)
					s.insert(1-active, 52500, 70, BMSStateIdle)
					s.run(10)
					if s.pack[0].State != BMSStateActive || s.pack[1].State != BMSStateActive {
						s.fail("compatible packs did not join in %s", state)
					}
				})
			}
		}
	}
}

func TestNonDriveRetainsAdmissionSafetyGates(t *testing.T) {
	for _, state := range nonDriveTestStates {
		for _, gate := range []string{"unconfirmed tag", "stale read", "read failure", "no pair", "skew", "load", "voltage", "own OFF", "peer OFF", "unknown peer", "role", "driving", "unknown state", "missing state"} {
			t.Run(fmt.Sprintf("%s/%s", state, gate), func(t *testing.T) {
				a := samplingPolicy()
				a.setVehicle(state)
				for step := 0; step < 3; step++ {
					deliverPair(t, a, admissionTestTime.Add(time.Duration(step)*admissionPollInterval), 52001)
				}
				now := admissionTestTime.Add(admissionStableTime + 200*time.Millisecond)
				switch gate {
				case "unconfirmed tag":
					a.tag(1, []byte{9})
					a.observe(1, admissionFrame(52001, BMSStateIdle), now)
				case "stale read":
					now = now.Add(admissionMaxAge + time.Second)
				case "read failure":
					a.invalidate(1)
				case "no pair":
					a.sampling.pairedID = 0
				case "skew":
					a.slots[1].observed = now.Add(-admissionMaxSkew - time.Second)
				case "load":
					a.slots[0].history[1].data.Current = admissionMaxCurrent + 1
				case "voltage":
					a.slots[1].frame.Voltage = 50000
				case "own OFF":
					a.slots[1].held = true
					a.offAttempt(1, now)
				case "peer OFF":
					a.offAttempt(0, now)
				case "unknown peer":
					a.slots[0].active = false
				case "role":
					a.setRole(1, false)
				case "driving":
					a.setVehicle(VehicleStateReadyToDrive)
				case "unknown state":
					a.setVehicle(VehicleStateOther)
				case "missing state":
					a.setVehicle("")
				}
				requireAdmission(t, a, 1, now, false)
			})
		}
	}
}

func TestNonDriveHandoverRequiresOldPackOff(t *testing.T) {
	for _, state := range nonDriveTestStates {
		for _, old := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/old%d", state, old), func(t *testing.T) {
				s := newAdmissionSimulation(t, "experimental")
				s.a.setVehicle(state)
				s.insert(old, 51000, 50, BMSStateActive)
				s.insert(1-old, 55000, 90, BMSStateIdle)
				s.run(12)
				if s.pack[old].State != BMSStateIdle || s.pack[1-old].State != BMSStateActive {
					s.fail("qualified handover did not finish in %s", state)
				}
			})
		}
	}
}

func TestNonDriveAdmissionDoesNotRequireMotion(t *testing.T) {
	for _, state := range nonDriveTestStates {
		t.Run(string(state), func(t *testing.T) {
			a := samplingPolicy()
			a.motion.enabled = true
			a.setVehicle(state)
			for step := 0; step < 3; step++ {
				deliverPair(t, a, admissionTestTime.Add(time.Duration(step)*admissionPollInterval), 52001)
			}
			requireAdmission(t, a, 1, admissionTestTime.Add(admissionStableTime+200*time.Millisecond), true)
		})
	}
}
