package battery

import (
	"fmt"
	"testing"
	"time"
)

func runningDualSimulation(t *testing.T, mode string) *admissionSimulation {
	t.Helper()
	s := newAdmissionSimulation(t, mode)
	s.a.setSeatbox(true)
	s.insert(0, 55000, 80, BMSStateIdle)
	s.insert(1, 55000, 80, BMSStateIdle)
	s.run(8)
	if s.pack[0].State != BMSStateActive || s.pack[1].State != BMSStateActive {
		s.fail("initial pair not running")
	}
	return s
}

func TestSimulationSameIdentityReinsertCannotReuseOn(t *testing.T) {
	for _, mode := range []string{"dual", "experimental"} {
		for index := 0; index < 2; index++ {
			for _, voltage := range []uint{50000, 54500, 60000} {
				t.Run(fmt.Sprintf("%s/slot%d/v%d", mode, index, voltage), func(t *testing.T) {
					s := runningDualSimulation(t, mode)
					cached := s.pack[index]
					s.a.setSeatbox(false)
					s.pack[index].Present = false
					s.a.observeAbsent(index, s.now)
					s.record("removed original identity slot=%d", index)
					// Same UID/SN, but the first NFC snapshot still contains the
					// pre-removal ACTIVE state and voltage.
					s.pack[index] = admissionFrame(voltage, BMSStateIdle)
					s.pack[index].SerialNumber = cached.SerialNumber
					s.pack[index].Charge = 30
					s.a.tag(index, []byte{byte(index + 1), s.generation[index]})
					s.a.observe(index, cached, s.now)
					s.a.setSeatbox(true)
					s.command(index, true)
					if s.pack[index].State == BMSStateActive {
						s.fail("same-identity reinsert reused ON permission")
					}
					s.command(1-index, true)
					if s.pack[1-index].State != BMSStateActive {
						s.fail("surviving pack was lost")
					}
					s.run(24)
					if s.pack[0].State != BMSStateActive && s.pack[1].State != BMSStateActive {
						s.fail("reinsertion left both packs inactive")
					}
					both := s.pack[0].State == BMSStateActive && s.pack[1].State == BMSStateActive
					if both != (voltage == 54500) {
						s.fail("unexpected qualified reinsertion result")
					}
				})
			}
		}
	}
}

func TestSimulationDriveContactLossPreservesEstablishedSupply(t *testing.T) {
	for _, mode := range []string{"dual", "experimental"} {
		for index := 0; index < 2; index++ {
			t.Run(fmt.Sprintf("%s/slot%d", mode, index), func(t *testing.T) {
				s := runningDualSimulation(t, mode)
				s.a.setVehicleAt(VehicleStateReadyToDrive, admissionTestTime.Add(-time.Hour))
				// RF loss is not evidence of electrical disconnection.
				s.a.observeAbsent(index, s.now)
				s.a.tag(index, []byte{byte(index + 1), s.generation[index]})
				for tick := 0; tick < 25; tick++ {
					s.observe()
					s.command(0, true)
					s.command(1, true)
					s.expireHeartbeat(0)
					s.expireHeartbeat(1)
					if s.pack[0].State != BMSStateActive || s.pack[1].State != BMSStateActive {
						s.fail("RF loss cut established drive supply")
					}
				}
				s.a.setVehicleAt(VehicleStateParked, admissionTestTime.Add(-time.Hour))
				if ok, _ := s.a.requestOn(index, s.now, s.limit); !ok {
					s.fail("parking revoked established supply permission")
				}
				// A later BMS update also restores measurement confirmation.
				s.pack[index].Voltage++
				s.run(8)
				if s.a.slots[index].requiresAdmission {
					s.fail("contact-loss qualification never recovered")
				}
			})
		}
	}
}

func TestSeatboxCancelsInFlightPairWithoutErasingIdentityConfirmation(t *testing.T) {
	a := runningSwapPolicy()
	a.sampling.enabled = true
	a.setSeatbox(true)
	a.offAttempt(1, admissionTestTime)
	idle := a.slots[1].frame
	idle.State = BMSStateIdle
	observeWritten(a, 1, idle, admissionTestTime.Add(time.Second))
	now := admissionTestTime.Add(2 * time.Second)
	id, _ := a.beginRound(now)
	for i := range a.slots {
		if a.claimRound(i, now) != id || id == 0 {
			t.Fatal("round not claimed")
		}
	}
	frames := [2]BMSData{a.slots[0].frame, a.slots[1].frame}
	a.observeAcquisition(0, id, acquisition{start: now, end: now.Add(100 * time.Millisecond), complete: now.Add(200 * time.Millisecond), data: frames[0]})
	written := a.slots[0].written
	a.setSeatbox(false)
	if a.slots[0].valid || a.slots[0].written != written {
		t.Fatal("freshness and identity confirmation were conflated")
	}
	a.observeAcquisition(1, id, acquisition{start: now, end: now.Add(100 * time.Millisecond), complete: now.Add(300 * time.Millisecond), data: frames[1]})
	if !a.sampling.pairedAt.IsZero() {
		t.Fatal("pre-seatbox round survived its boundary")
	}
	requireAdmission(t, a, 1, now.Add(300*time.Millisecond), false)
	requireAdmission(t, a, 0, now.Add(300*time.Millisecond), true)
}

func TestSimulationReplacementWithMissingOrLatePeer(t *testing.T) {
	for _, mode := range []string{"dual", "experimental"} {
		for _, failure := range []string{"missing-pair", "read-error", "stale-peer"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				s := runningDualSimulation(t, mode)
				s.a.setSeatbox(false)
				s.insert(1, 50000, 20, BMSStateIdle)
				// Even a changing cached ACTIVE report must not cause ON to an
				// independently idle replacement without valid paired readings.
				reported := s.pack[1]
				reported.State = BMSStateActive
				observeWritten(s.a, 1, reported, s.now)
				switch failure {
				case "read-error":
					s.a.invalidate(0)
				case "stale-peer":
					s.advance(admissionMaxAge + time.Second)
				}
				s.command(1, true)
				if s.pack[1].State == BMSStateActive {
					s.fail("replacement joined with %s", failure)
				}
				s.command(0, true)
				if s.pack[0].State != BMSStateActive {
					s.fail("peer failure cut established supply")
				}
			})
		}
	}
}

func TestSimulationSwapScenarioMatrix(t *testing.T) {
	for _, mode := range []string{"dual", "experimental"} {
		for _, keep := range []bool{false, true} {
			for index := 0; index < 2; index++ {
				for _, voltage := range []uint{50000, 54500, 55500, 60000} {
					for _, deliveredOff := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/keep=%t/slot%d/v%d/off=%t", mode, keep, index, voltage, deliveredOff), func(t *testing.T) {
							s := runningDualSimulation(t, mode)
							s.a.setSeatbox(false)
							if !keep {
								for i := range s.pack {
									s.a.offAttempt(i, s.now)
									if deliveredOff {
										s.pack[i].State = BMSStateIdle
									}
								}
							}
							s.pack[index].Present = false
							s.a.observeAbsent(index, s.now)
							s.insert(index, voltage, 30, BMSStateIdle)
							// Neither cached frames nor a failed acquisition can grant ON.
							s.a.invalidate(index)
							s.command(index, false)
							s.a.setSeatbox(true)
							if !keep && !deliveredOff {
								for i := range s.pack {
									if !s.a.slots[i].offPending {
										s.fail("lost unconfirmed OFF")
									}
								}
								s.advance(time.Second)
								for i := range s.pack {
									s.pack[i].State = BMSStateIdle
								}
							}
							s.run(24)
							if s.pack[0].State != BMSStateActive && s.pack[1].State != BMSStateActive {
								s.fail("swap left both packs inactive")
							}
							if voltage >= 54500 && voltage <= 55500 {
								if s.pack[0].State != BMSStateActive || s.pack[1].State != BMSStateActive {
									s.fail("compatible swap did not recover dual operation")
								}
							} else if s.pack[0].State == BMSStateActive && s.pack[1].State == BMSStateActive {
								s.fail("mismatched swap ended in parallel operation")
							}
						})
					}
				}
			}
		}
	}
}
