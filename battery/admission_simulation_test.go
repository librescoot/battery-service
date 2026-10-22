package battery

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// Exercise the production policy against separate simulated pack states and
// transport outcomes. Observations arrive through the real paired-read API;
// command permission is not treated as proof that a command reached the BMS.
type admissionSimulation struct {
	t           *testing.T
	a           *packAdmission
	now         time.Time
	limit       uint64
	pack        [2]BMSData
	generation  [2]byte
	lastFrame   [2]BMSData
	haveFrame   [2]bool
	changedAt   [2]time.Time
	onPending   [2]time.Time
	offPending  [2]time.Time
	onCommandAt [2]time.Time
	heartbeat   [2]time.Time
	stationary  bool
	trace       []string
}

func newAdmissionSimulation(t *testing.T, mode string) *admissionSimulation {
	t.Helper()
	role1 := BatteryRoleInactive
	if mode != "single" {
		role1 = BatteryRoleActive
	}
	a := newPackAdmission(&BatteryConfiguration{Readers: []BatteryReaderConfig{
		{Index: 0, Enabled: true, Role: BatteryRoleActive},
		{Index: 1, Enabled: true, Role: role1},
	}})
	a.conservative = mode == "experimental"
	a.sampling.enabled = true
	a.setVehicle(VehicleStateParked)
	return &admissionSimulation{t: t, a: a, now: admissionTestTime, limit: 1000}
}

func (s *admissionSimulation) record(format string, args ...any) {
	s.trace = append(s.trace, fmt.Sprintf("%s %s", s.now.Sub(admissionTestTime), fmt.Sprintf(format, args...)))
	if len(s.trace) > 80 {
		s.trace = s.trace[len(s.trace)-80:]
	}
}

func (s *admissionSimulation) fail(format string, args ...any) {
	s.t.Helper()
	s.t.Fatalf("%s\ntrace:\n%s", fmt.Sprintf(format, args...), strings.Join(s.trace, "\n"))
}

func (s *admissionSimulation) insert(index int, voltage, soc uint, state BMSState) {
	s.generation[index]++
	s.haveFrame[index], s.changedAt[index] = false, time.Time{}
	s.pack[index] = admissionFrame(voltage, state)
	s.pack[index].Charge, s.pack[index].LowSOC = soc, soc == 0
	s.heartbeat[index] = s.now
	s.a.tag(index, []byte{byte(index + 1), s.generation[index]})
	s.record("insert slot=%d voltage=%d soc=%d state=%s", index, voltage, soc, state)
}

func (s *admissionSimulation) advance(d time.Duration) {
	end := s.now.Add(d)
	for s.now.Before(end) {
		s.now = s.now.Add(min(100*time.Millisecond, end.Sub(s.now)))
		if s.a.motion.enabled {
			s.a.observeMotion(s.stationary, s.now, s.now)
		}
	}
}

func (s *admissionSimulation) observe() {
	s.advance(admissionPollInterval)
	both := s.pack[0].Present && s.pack[1].Present
	var id uint64
	if both {
		id, _ = s.a.beginRound(s.now)
		if id != 0 {
			for i := range s.pack {
				if s.a.claimRound(i, s.now) != id {
					s.fail("sampling claim failed")
				}
			}
		}
	}
	for i := range s.pack {
		if !s.pack[i].Present {
			s.a.observeAbsent(i, s.now)
			continue
		}
		frame := s.pack[i]
		if id != 0 {
			s.a.observeAcquisition(i, id, acquisition{start: s.now, end: s.now.Add(100 * time.Millisecond), complete: s.now.Add(200 * time.Millisecond), data: frame})
		} else {
			s.a.observe(i, frame, s.now.Add(200*time.Millisecond))
		}
		s.noteEvidence(i, frame, s.now.Add(200*time.Millisecond))
	}
	s.advance(201 * time.Millisecond)
	s.record("observe states=[%s,%s] voltages=[%d,%d]", s.pack[0].State, s.pack[1].State, s.pack[0].Voltage, s.pack[1].Voltage)
	s.checkReservations()
}

func (s *admissionSimulation) noteEvidence(i int, frame BMSData, at time.Time) {
	previous := s.lastFrame[i]
	if s.haveFrame[i] && (frame.State != previous.State || frame.Voltage != previous.Voltage ||
		frame.Current != previous.Current || frame.Charge != previous.Charge || frame.FaultCode != previous.FaultCode) {
		s.changedAt[i] = at
	}
	s.lastFrame[i], s.haveFrame[i] = frame, true
	if frame.State == BMSStateActive {
		if !s.onPending[i].IsZero() && !s.a.slots[i].held {
			s.fail("confirmed ON lost its reservation: slot=%d", i)
		}
		s.onPending[i] = time.Time{}
	} else {
		if !s.onPending[i].IsZero() && s.changedAt[i].After(s.onCommandAt[i]) && at.Sub(s.onPending[i]) >= admissionOnSettle {
			s.onPending[i] = time.Time{}
		}
		if !s.offPending[i].IsZero() && !s.offPending[i].Before(s.onCommandAt[i]) &&
			s.changedAt[i].After(s.offPending[i]) && at.Sub(s.offPending[i]) >= BMSTimeCmd {
			s.offPending[i], s.onPending[i] = time.Time{}, time.Time{}
		}
	}
}

func (s *admissionSimulation) checkReservations() {
	for i := range s.pack {
		if (!s.onPending[i].IsZero() || !s.offPending[i].IsZero()) && !s.a.slots[i].held {
			s.fail("uncertain command reservation lost: slot=%d on=%v off=%v", i, s.onPending[i], s.offPending[i])
		}
	}
}

// delivered=false covers a failed or ambiguous transport without a BMS state
// change. The real coordinator must retain the same possible-live reservation.
func (s *admissionSimulation) command(index int, delivered bool) {
	if !s.pack[index].Present {
		return // Discovery fails before the production NFC write gate.
	}
	p, peer := &s.pack[index], &s.pack[1-index]
	forced := s.a.shouldIsolate(index, s.now, s.limit)
	explicitOff := !s.a.slots[index].eligible || (p.LowSOC && p.Charge == 0)
	if forced || explicitOff {
		if forced && !explicitOff {
			if !s.a.isolationOffAttempt(index, s.now) {
				s.record("OFF deferred slot=%d", index)
				return
			}
			e := s.a.isolation
			if !isolationVehicle(s.a.vehicle) || e == nil {
				s.fail("corrective OFF outside allowed vehicle state")
			}
			if p.State == BMSStateActive && (!peer.Present || peer.State != BMSStateActive) {
				if !e.handover || !s.a.conservative || !peer.Present || peer.FaultCode != 0 || (peer.LowSOC && peer.Charge == 0) {
					s.fail("unintended last-supply cutoff: slot=%d", index)
				}
			}
		} else {
			s.a.offAttempt(index, s.now)
		}
		if s.a.slots[index].held && (s.offPending[index].IsZero() || s.onCommandAt[index].After(s.offPending[index])) {
			s.offPending[index] = s.now
		}
		s.record("OFF slot=%d delivered=%t forced=%t", index, delivered, forced)
		if delivered {
			p.State = BMSStateIdle
		}
	} else {
		ok, reason := s.a.requestOn(index, s.now, s.limit)
		s.record("ON slot=%d allowed=%t reason=%q delivered=%t", index, ok, reason, delivered)
		if !ok {
			return
		}
		if !s.a.slots[index].eligible {
			s.fail("ON authorized for disabled slot %d", index)
		}
		if p.State != BMSStateActive {
			if peer.Present && peer.State == BMSStateActive && s.limit > 0 && voltageDistance(p.Voltage, peer.Voltage) > s.limit {
				s.fail("incompatible parallel activation: slot=%d delta=%d limit=%d", index, voltageDistance(p.Voltage, peer.Voltage), s.limit)
			}
			if !s.offPending[index].IsZero() {
				s.fail("ON authorized before OFF confirmation")
			}
			s.onPending[index] = s.now
		}
		s.onCommandAt[index] = s.now
		if delivered {
			s.heartbeat[index] = s.now
			p.State = BMSStateActive
		}
	}
	s.checkReservations()
}

// A simulated watchdog exercises missing keepalives, not a claimed hardware
// timeout value. Its resulting BMS status is delivered through observe.
func (s *admissionSimulation) expireHeartbeat(index int) {
	if s.pack[index].State != BMSStateActive || s.now.Sub(s.heartbeat[index]) < time.Minute {
		return
	}
	s.pack[index].State = BMSStateIdle
	frame := s.pack[index]
	s.a.observe(index, frame, s.now)
	s.noteEvidence(index, frame, s.now)
	s.record("BMS heartbeat expired slot=%d", index)
}

func (s *admissionSimulation) run(ticks int) {
	for i := 0; i < ticks; i++ {
		s.observe()
		// Alternate callback order to avoid relying on the front reader winning.
		first := i % 2
		s.command(first, true)
		s.command(1-first, true)
	}
}

func TestSimulationModeMatrix(t *testing.T) {
	for _, mode := range []string{"single", "dual", "experimental"} {
		for _, first := range []int{0, 1} {
			for _, delta := range []int{-4000, -1001, -1000, -900, 0, 900, 1000, 1001, 4000} {
				for _, soc := range []uint{0, 1, 50, 100} {
					t.Run(fmt.Sprintf("%s/first%d/delta%d/soc%d", mode, first, delta, soc), func(t *testing.T) {
						s := newAdmissionSimulation(t, mode)
						s.insert(first, 53000, 50, BMSStateIdle)
						s.run(5)
						s.insert(1-first, uint(53000+delta), soc, BMSStateIdle)
						s.run(24)
						if mode == "single" {
							if s.pack[1].State == BMSStateActive || (s.pack[0].Charge > 0 && s.pack[0].State != BMSStateActive) {
								s.fail("single mode did not select only slot 0")
							}
						} else if mode == "experimental" && soc > 0 {
							if delta > 1000 && (s.pack[1-first].State != BMSStateActive || s.pack[first].State == BMSStateActive) {
								s.fail("higher incoming pack did not take over")
							}
							if delta >= -900 && delta <= 900 && (s.pack[0].State != BMSStateActive || s.pack[1].State != BMSStateActive) {
								s.fail("compatible packs did not both activate")
							}
						}
					})
				}
			}
		}
	}
}

func TestSimulationStartupAndZeroLimit(t *testing.T) {
	for _, limit := range []uint64{0, 1000} {
		for _, state0 := range []BMSState{BMSStateIdle, BMSStateActive} {
			for _, state1 := range []BMSState{BMSStateIdle, BMSStateActive} {
				for _, mode := range []string{"single", "dual", "experimental"} {
					t.Run(fmt.Sprintf("%s/limit%d/%s/%s", mode, limit, state0, state1), func(t *testing.T) {
						s := newAdmissionSimulation(t, mode)
						s.limit = limit
						s.insert(0, 51000, 95, state0)
						s.insert(1, 55000, 10, state1)
						s.run(24)
						if mode == "single" && s.pack[1].State == BMSStateActive {
							s.fail("single mode left slot 1 active")
						}
						if mode == "experimental" && limit > 0 && (s.pack[0].State == BMSStateActive || s.pack[1].State != BMSStateActive) {
							s.fail("voltage priority did not override SOC")
						}
						if mode != "single" && limit == 0 && (s.pack[0].State != BMSStateActive || s.pack[1].State != BMSStateActive) {
							s.fail("zero voltage limit blocked otherwise qualified parallel operation")
						}
					})
				}
			}
		}
	}
}

func TestSimulationConvergenceAndStoppedQualification(t *testing.T) {
	for _, stoppedGate := range []bool{false, true} {
		t.Run(fmt.Sprintf("stoppedGate=%t", stoppedGate), func(t *testing.T) {
			s := newAdmissionSimulation(t, "experimental")
			s.a.motion.enabled = stoppedGate
			s.insert(0, 53000, 80, BMSStateActive)
			s.insert(1, 49000, 40, BMSStateIdle)
			s.run(6)
			s.a.setVehicle(VehicleStateReadyToDrive)
			s.pack[1].Voltage = 52500
			s.run(6)
			if s.pack[1].State == BMSStateActive {
				s.fail("second pack joined while moving")
			}
			if stoppedGate {
				s.stationary = true
				s.run(1)
				if s.pack[1].State == BMSStateActive {
					s.fail("stopped gate skipped its qualification window")
				}
			} else {
				s.a.setVehicle(VehicleStateParked)
			}
			s.run(9)
			if s.pack[0].State != BMSStateActive || s.pack[1].State != BMSStateActive {
				s.fail("compatible settled pair did not activate")
			}
		})
	}
}

func TestSimulationLoadAndVoltageSagMustSettle(t *testing.T) {
	for _, disturbance := range []string{"current", "voltage", "fault"} {
		t.Run(disturbance, func(t *testing.T) {
			s := newAdmissionSimulation(t, "experimental")
			s.insert(0, 53000, 80, BMSStateActive)
			s.insert(1, 52500, 70, BMSStateIdle)
			for tick := 0; tick < 8; tick++ {
				switch disturbance {
				case "current":
					s.pack[0].Current = 10000
				case "voltage":
					s.pack[0].Voltage = uint(53000 + (tick%2)*200)
				case "fault":
					s.pack[1].FaultCode = 1
				}
				s.run(1)
				if s.pack[1].State == BMSStateActive {
					s.fail("second pack joined during %s disturbance", disturbance)
				}
			}
			s.pack[0].Current, s.pack[0].Voltage, s.pack[1].FaultCode = 0, 53000, 0
			s.run(9)
			if s.pack[1].State != BMSStateActive {
				s.fail("second pack did not join after %s settled", disturbance)
			}
		})
	}
}

func TestSimulationFailedHandoverKeepsSoleSupplyWhileDriving(t *testing.T) {
	for _, old := range []int{0, 1} {
		t.Run(fmt.Sprintf("old%d", old), func(t *testing.T) {
			s := newAdmissionSimulation(t, "experimental")
			s.insert(old, 51000, 50, BMSStateActive)
			s.insert(1-old, 55000, 90, BMSStateIdle)
			for i := 0; i < 4; i++ {
				s.observe()
			}
			s.command(old, false)
			if s.a.isolation == nil || !s.a.isolation.attempted {
				s.fail("handover did not attempt OFF")
			}
			s.a.setVehicle(VehicleStateReadyToDrive)
			for i := 0; i < 40; i++ {
				s.observe()
				s.command(old, true)
				s.command(1-old, true)
				s.expireHeartbeat(old)
				if s.pack[old].State != BMSStateActive || s.pack[1-old].State == BMSStateActive {
					s.fail("paused handover lost its sole supply or enabled the target")
				}
			}
			s.a.setVehicle(VehicleStateParked)
			s.run(12)
			if s.pack[old].State == BMSStateActive || s.pack[1-old].State != BMSStateActive {
				s.fail("handover did not finish after parking")
			}
		})
	}
}

func TestSimulationRemovedHandoverTargetRestoresOldPack(t *testing.T) {
	for _, old := range []int{0, 1} {
		s := newAdmissionSimulation(t, "experimental")
		s.insert(old, 51000, 50, BMSStateActive)
		s.insert(1-old, 55000, 90, BMSStateIdle)
		for i := 0; i < 4; i++ {
			s.observe()
		}
		s.command(old, true)
		s.observe()
		if s.a.isolation == nil || !s.a.isolation.offConfirmed {
			s.fail("missing victim OFF proof")
		}
		s.pack[1-old].Present = false
		s.pack[1-old].State = BMSStateUnknown
		s.a.observeAbsent(1-old, s.now)
		s.run(6)
		if s.pack[old].State != BMSStateActive {
			s.fail("removed target left the old pack permanently OFF")
		}
	}
}

func TestSimulationSeededEvents(t *testing.T) {
	for seed := int64(0); seed < 256; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			s := newAdmissionSimulation(t, "experimental")
			s.insert(0, 53000, 80, BMSStateIdle)
			s.insert(1, 52500, 70, BMSStateIdle)
			for step := 0; step < 256; step++ {
				i := rng.Intn(2)
				s.record("event step=%d slot=%d", step, i)
				switch rng.Intn(13) {
				case 0, 1:
					s.observe()
				case 2, 3:
					s.command(i, rng.Intn(3) != 0)
				case 4:
					s.a.invalidate(i)
					s.record("read failure slot=%d", i)
				case 5:
					s.a.setVehicle([]VehicleState{VehicleStateParked, VehicleStateStandby, VehicleStateReadyToDrive}[rng.Intn(3)])
					s.record("vehicle=%s", s.a.vehicle)
				case 6:
					s.a.setRole(1, rng.Intn(2) == 0)
					s.record("slot1 eligible=%t", s.a.slots[1].eligible)
				case 7:
					if s.pack[i].Present {
						s.pack[i].Present = false
						s.pack[i].State = BMSStateUnknown
						s.a.observeAbsent(i, s.now)
						s.record("removed slot=%d", i)
					} else {
						s.insert(i, uint(50000+rng.Intn(6)*1000), uint(1+rng.Intn(100)), BMSStateIdle)
					}
				case 8:
					s.limit = []uint64{0, 500, 1000, 2000}[rng.Intn(4)]
					s.record("limit=%d", s.limit)
				case 9:
					s.advance(11 * time.Second)
					s.record("evidence expired")
				case 10:
					if s.pack[i].Present {
						s.generation[i]++
						s.haveFrame[i], s.changedAt[i] = false, time.Time{}
						s.a.tag(i, []byte{byte(i + 1), s.generation[i]})
						s.record("tag changed slot=%d", i)
					}
				case 12:
					s.advance(time.Minute)
					s.expireHeartbeat(i)
				case 11:
					// A changed input is delivered as an unpaired observation;
					// it cannot borrow a previous round's peer measurement.
					if s.pack[i].Present {
						s.pack[i].Voltage = uint(50000 + rng.Intn(6)*1000)
						s.pack[i].FaultCode = uint(rng.Intn(3) / 2)
						frame := s.pack[i]
						s.a.observe(i, frame, s.now)
						s.noteEvidence(i, frame, s.now)
						s.record("unpaired change slot=%d v=%d fault=%d", i, s.pack[i].Voltage, s.pack[i].FaultCode)
					}
				}
				s.checkReservations()
			}
		})
	}
}
