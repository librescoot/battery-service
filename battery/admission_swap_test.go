package battery

import (
	"fmt"
	"testing"
	"time"

	"battery-service/battery/fsm"
)

func runningSwapPolicy() *packAdmission {
	a := newAdmissionTestPolicy()
	for i := 0; i < 2; i++ {
		a.tag(i, []byte{byte(i + 1)})
		data := admissionFrame(55000, BMSStateActive)
		data.SerialNumber = fmt.Sprintf("original-%d", i)
		observeWritten(a, i, data, admissionTestTime)
	}
	return a
}

func TestReplacementActiveStatusCannotBypassAdmission(t *testing.T) {
	for _, conservative := range []bool{false, true} {
		for _, identity := range []string{"tag", "serial"} {
			for index := 0; index < 2; index++ {
				t.Run(fmt.Sprintf("conservative=%t/%s/slot%d", conservative, identity, index), func(t *testing.T) {
					a := runningSwapPolicy()
					a.conservative = conservative
					a.sampling.enabled = true
					now := admissionTestTime.Add(2 * time.Second)
					if identity == "tag" {
						a.tag(index, []byte{9})
					}
					replacement := admissionFrame(50000, BMSStateActive)
					replacement.SerialNumber = "replacement"
					observeWritten(a, index, replacement, now)
					if admissionTestReader(a, index).admitCommand(fsm.BMSCmdOn, now) {
						t.Fatal("replacement active status bypassed paired voltage admission")
					}
					requireAdmission(t, a, 1-index, now, true)
					if !a.slots[index].held {
						t.Fatal("lost possible-live power reservation")
					}
				})
			}
		}
	}
}

func TestReplacementCannotAuthorizeSurvivorOff(t *testing.T) {
	a := runningSwapPolicy()
	now := admissionTestTime.Add(2 * time.Second)
	a.tag(1, []byte{9})
	replacement := admissionFrame(59000, BMSStateActive)
	replacement.SerialNumber = "replacement"
	observeWritten(a, 1, replacement, now)
	original := a.slots[0].frame
	a.observe(0, original, now)
	if a.shouldIsolate(0, now, DefaultMaxVoltageDeltaMV) {
		t.Fatal("unqualified replacement authorized OFF of the established supply")
	}
	requireAdmission(t, a, 0, now, true)
	requireAdmission(t, a, 1, now, false)
}

func TestSwapSerialChangeNeedsItsOwnFreshFrame(t *testing.T) {
	a := runningSwapPolicy()
	replacement := admissionFrame(50000, BMSStateActive)
	replacement.SerialNumber = "replacement"
	a.observe(1, replacement, admissionTestTime.Add(time.Second))
	if a.slots[1].valid || !a.slots[1].written.IsZero() {
		t.Fatal("different pack's frame confirmed freshness against its predecessor")
	}
}

func TestSeatboxInvalidatesAdmissionBeforeReadersRun(t *testing.T) {
	values := map[string]string{"vehicle/seatbox:lock": "closed"}
	s, _ := admissionSettingService(t, values)
	s.admission = runningSwapPolicy()
	for _, reader := range s.readers {
		reader.seatboxLockChan = make(chan bool, 1)
	}
	s.handleSeatboxUpdate()
	frames := [2]BMSData{s.admission.slots[0].frame, s.admission.slots[1].frame}
	for _, state := range []string{"open", "closed"} {
		values["vehicle/seatbox:lock"] = state
		s.handleSeatboxUpdate()
		for i := range s.admission.slots {
			slot := &s.admission.slots[i]
			if slot.valid || slot.observedRound != 0 || len(slot.history) != 0 {
				t.Fatalf("seatbox %s retained measurement evidence for slot %d", state, i)
			}
			if !slot.held || slot.requiresAdmission {
				t.Fatal("lost established power reservation or permission")
			}
			observeWritten(s.admission, i, frames[i], admissionTestTime.Add(2*time.Second))
		}
	}
	// Duplicate closed notifications must not prevent fresh data from settling.
	s.handleSeatboxUpdate()
	if !s.admission.slots[0].valid {
		t.Fatal("duplicate seatbox status invalidated fresh readings")
	}
}

func TestSeatboxInvalidationPreservesSupplyAcrossParking(t *testing.T) {
	values := map[string]string{"vehicle/seatbox:lock": "closed"}
	s, _ := admissionSettingService(t, values)
	a := runningSwapPolicy()
	s.admission = a
	for _, reader := range s.readers {
		reader.seatboxLockChan = make(chan bool, 1)
	}
	a.setVehicle(VehicleStateReadyToDrive)
	s.handleSeatboxUpdate()
	values["vehicle/seatbox:lock"] = "open"
	s.handleSeatboxUpdate()
	for i := range a.slots {
		if a.slots[i].valid {
			t.Fatal("drive latch preserved stale measurement evidence")
		}
		requireAdmission(t, a, i, admissionTestTime.Add(time.Second), true)
	}
	a.setVehicle(VehicleStateParked)
	for i := range a.slots {
		requireAdmission(t, a, i, admissionTestTime.Add(2*time.Second), true)
	}
}

func TestReplacementMustPassFreshPairedVoltageGate(t *testing.T) {
	for _, conservative := range []bool{false, true} {
		for _, voltage := range []uint{50000, 54500, 59000} {
			for index := 0; index < 2; index++ {
				t.Run(fmt.Sprintf("conservative=%t/voltage=%d/slot%d", conservative, voltage, index), func(t *testing.T) {
					a := runningSwapPolicy()
					a.conservative = conservative
					a.sampling.enabled = true
					a.tag(index, []byte{9})
					frames := [2]BMSData{a.slots[0].frame, a.slots[1].frame}
					frames[index] = admissionFrame(voltage, BMSStateActive)
					frames[index].SerialNumber = "replacement"
					observeWritten(a, index, frames[index], admissionTestTime.Add(time.Second))
					var now time.Time
					for round := 0; round < 3; round++ {
						start := admissionTestTime.Add(time.Duration(2+round*3) * time.Second)
						id, _ := a.beginRound(start)
						for slot, data := range frames {
							data.Voltage += uint(round % 2)
							if id == 0 || a.claimRound(slot, start) != id {
								t.Fatal("round not claimable")
							}
							now = start.Add(200 * time.Millisecond)
							a.observeAcquisition(slot, id, acquisition{start: start, end: start.Add(100 * time.Millisecond), complete: now, data: data})
						}
						if a.sampling.pairedID != id {
							t.Fatal("fresh pair not accepted")
						}
					}
					want := voltage == 54500
					requireAdmission(t, a, index, now, want)
					requireAdmission(t, a, 1-index, now, true)
					if want {
						if a.slots[index].requiresAdmission {
							t.Fatal("qualification never completed")
						}
						if !a.sampling.pairedAt.IsZero() {
							t.Fatal("ON retained pre-command pair")
						}
						a.setVehicle(VehicleStateReadyToDrive)
						requireAdmission(t, a, index, now.Add(time.Minute), true)
					}
				})
			}
		}
	}
}

func TestSwapRetainsUnresolvedOff(t *testing.T) {
	a := runningSwapPolicy()
	now := admissionTestTime.Add(time.Second)
	a.offAttempt(1, now)
	a.tag(1, []byte{9})
	replacement := admissionFrame(54500, BMSStateActive)
	replacement.SerialNumber = "replacement"
	observeWritten(a, 1, replacement, now.Add(time.Second))
	if !a.slots[1].offPending || !a.slots[1].held {
		t.Fatal("swap discarded unresolved OFF")
	}
	requireAdmission(t, a, 1, now.Add(time.Second), false)
}

func TestSeatboxKeepsSerialIdentityWhileDiscardingFrames(t *testing.T) {
	a := runningSwapPolicy()
	a.setSeatbox(true)
	a.setVehicle(VehicleStateReadyToDrive)
	a.setSeatbox(false)
	replacement := admissionFrame(50000, BMSStateActive)
	replacement.SerialNumber = "replacement"
	observeWritten(a, 1, replacement, admissionTestTime.Add(2*time.Second))
	requireAdmission(t, a, 1, admissionTestTime.Add(2*time.Second), false)
	requireAdmission(t, a, 0, admissionTestTime.Add(2*time.Second), true)
}

func TestSamePackRediscoveryRetainsRunningHeartbeat(t *testing.T) {
	a := runningSwapPolicy()
	a.setVehicle(VehicleStateReadyToDrive)
	a.invalidate(1)
	a.tag(1, []byte{2})
	requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), true)
	requireAdmission(t, a, 1, admissionTestTime.Add(time.Second), true)
}
