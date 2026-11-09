package battery

import (
	"fmt"
	"testing"
	"time"
)

func TestStorageReaderFailureDoesNotStarveActivation(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, state := range []VehicleState{VehicleStateStandby, VehicleStateParked} {
			t.Run(fmt.Sprintf("strict=%t/%s", strict, state), func(t *testing.T) {
				a := storagePeerPolicy()
				a.conservative, a.sampling.enabled = strict, true
				a.setVehicleAt(state, admissionTestTime.Add(-time.Hour))
				frame := admissionFrame(57400, BMSStateIdle)
				observeWritten(a, 0, frame, admissionTestTime.Add(-2*admissionMaxAge))
				for tick := 0; tick < 10; tick++ {
					now := admissionTestTime.Add(time.Duration(tick) * admissionPollInterval)
					id, _ := a.beginRound(now)
					if id == 0 {
						continue
					}
					// The reader FSM attempts ON before reading status. Reader 1
					// cannot initialize, so it never claims or completes its read.
					if ok, _ := a.requestOn(0, now.Add(BMSTimeCmd), DefaultMaxVoltageDeltaMV); ok {
						if tick != 1 {
							t.Fatalf("activation on tick %d, want first retry", tick)
						}
						return
					}
					start := now.Add(2 * BMSTimeCmd)
					if a.claimRound(0, start) != id {
						t.Fatal("healthy reader could not claim the round")
					}
					a.observeAcquisition(0, id, acquisition{
						start: start, end: start.Add(50 * time.Millisecond),
						complete: start.Add(100 * time.Millisecond), data: frame,
					})
				}
				t.Fatal("healthy reader never activated despite successful status reads")
			})
		}
	}
}

func TestSingleSupplyRoundNotifiesAdmissionDemand(t *testing.T) {
	a := storagePeerPolicy()
	a.sampling.enabled = true
	frame := admissionFrame(57400, BMSStateIdle)
	observeWritten(a, 0, frame, admissionTestTime.Add(-2*admissionMaxAge))
	id, _ := a.beginRound(admissionTestTime)
	if id == 0 || a.claimRound(0, admissionTestTime) != id {
		t.Fatal("sampling round unavailable")
	}
	if !a.observeAcquisition(0, id, acquisition{
		start: admissionTestTime, end: admissionTestTime.Add(50 * time.Millisecond),
		complete: admissionTestTime.Add(100 * time.Millisecond), data: frame,
	}) {
		t.Fatal("renewed own data did not request a foreground activation retry")
	}
	if a.roundInProgress() || !a.sampling.pairedAt.IsZero() {
		t.Fatal("single-supply acquisition left a pending round or fabricated a pair")
	}
}

func TestPowerTrackedPeerStillRequiresPairedSampling(t *testing.T) {
	for _, evidence := range []string{"eligible", "formerly eligible", "unconfirmed active", "pending ON", "pending OFF"} {
		t.Run(evidence, func(t *testing.T) {
			a := storagePeerPolicy()
			a.sampling.enabled = true
			switch evidence {
			case "eligible":
				a.setRole(1, true)
			case "formerly eligible":
				a.setRole(1, true)
				a.setRole(1, false)
			case "unconfirmed active":
				a.observe(1, admissionFrame(50000, BMSStateActive), admissionTestTime)
			case "pending ON":
				a.slots[1].held = true
			case "pending OFF":
				a.slots[1].offPending = true
			}
			frame := admissionFrame(57400, BMSStateIdle)
			observeWritten(a, 0, frame, admissionTestTime.Add(-time.Second))
			id, _ := a.beginRound(admissionTestTime)
			if id == 0 || a.claimRound(0, admissionTestTime) != id {
				t.Fatal("sampling round unavailable")
			}
			a.observeAcquisition(0, id, acquisition{
				start: admissionTestTime, end: admissionTestTime.Add(50 * time.Millisecond),
				complete: admissionTestTime.Add(100 * time.Millisecond), data: frame,
			})
			if !a.roundInProgress() || !a.sampling.pairedAt.IsZero() {
				t.Fatal("required peer was bypassed or a pair was fabricated")
			}
			requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), false)
		})
	}
}
