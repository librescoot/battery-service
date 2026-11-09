package battery

import (
	"fmt"
	"testing"
	"time"
)

func stoppedSwapPolicy(t *testing.T, strict, stopped bool, keep string, target int, change string) (*packAdmission, time.Time) {
	t.Helper()
	s, _ := admissionSettingService(t, map[string]string{
		"settings/experimental.battery.dual-pack-admission": fmt.Sprint(strict),
		"settings/" + stoppedAdmissionSetting:               fmt.Sprint(stopped),
	})
	s.batteryConfig.Readers[1].Role = BatteryRoleActive
	s.config.KeepActiveOnSeatboxOpen.Store(keep == "manual")
	s.config.AuxLowKeepActive.Store(keep == "aux")
	s.loadAdmissionSetting()
	a := s.admission
	a.setVehicleAt(VehicleStateParked, admissionTestTime.Add(-time.Hour))
	a.setSeatbox(true)
	var frames [2]BMSData
	for i := range frames {
		frames[i] = admissionFrame(55000, BMSStateActive)
		if i == target {
			frames[i].State = BMSStateIdle
			frames[i].Voltage = 54500
		}
		frames[i].SerialNumber = fmt.Sprintf("original-%d", i)
		a.tag(i, []byte{byte(i + 1)})
		observeWritten(a, i, frames[i], admissionTestTime)
	}
	a.setVehicleAt(VehicleStateReadyToDrive, admissionTestTime.Add(-time.Hour))
	now := admissionTestTime.Add(time.Second)
	// One reading of both packs, after the ECU has reported standstill. The
	// evidence is withdrawn again so callers see the drive-state gate.
	window := func(start time.Time, _ bool) time.Time {
		t.Helper()
		standstillBefore(a, start, 2*admissionStillTime)
		a.sampling.pairedID = 0
		id, _ := a.beginRound(start)
		for i := range frames {
			if id == 0 || a.claimRound(i, start) != id {
				t.Fatal("pair not claimed")
			}
			a.observeAcquisition(i, id, acquisition{start: start, end: start.Add(100 * time.Millisecond), complete: start.Add(200 * time.Millisecond), data: frames[i]})
		}
		a.invalidateMotion()
		return start.Add(200 * time.Millisecond)
	}
	now = window(now, false)
	a.setSeatbox(false)
	requireAdmission(t, a, target, now, false)
	if change != "seatbox" {
		for i := range frames {
			if i != target && change != "both-identities" {
				continue
			}
			if change != "serial" {
				a.tag(i, []byte{byte(i + 9)})
			}
			frames[i].SerialNumber = fmt.Sprintf("replacement-%d", i)
			first := frames[i]
			first.Voltage--
			a.observe(i, first, now)
		}
	}
	a.setSeatbox(true)
	requireAdmission(t, a, target, now, false)
	now = window(now.Add(time.Second), true)
	// No standstill evidence is left for the callers' drive-state checks.
	a.invalidateMotion()
	return a, now
}

func TestStoppedSwapSettingsMatrix(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, stopped := range []bool{false, true} {
			for _, keep := range []string{"off", "manual", "aux"} {
				for target := 0; target < 2; target++ {
					for _, change := range []string{"seatbox", "tag", "serial"} {
						t.Run(fmt.Sprintf("strict=%t/stopped=%t/keep=%s/slot%d/%s", strict, stopped, keep, target, change), func(t *testing.T) {
							a, now := stoppedSwapPolicy(t, strict, stopped, keep, target, change)
							requireAdmission(t, a, target, now, false)
							requireAdmission(t, a, 1-target, now, true)
							a.setVehicleAt(VehicleStateParked, now)
							requireAdmission(t, a, target, now, false)
							now = requalifyAfterParking(t, a, now)
							requireAdmission(t, a, target, now, true)
							requireAdmission(t, a, 1-target, now, true)
						})
					}
				}
			}
		}
	}
}

func TestStoppedSwapRequiresAdmittedRunningPeer(t *testing.T) {
	for _, keep := range []string{"off", "manual", "aux"} {
		for target := 0; target < 2; target++ {
			t.Run(fmt.Sprintf("%s/slot%d", keep, target), func(t *testing.T) {
				a, now := stoppedSwapPolicy(t, true, true, keep, target, "both-identities")
				peer := &a.slots[1-target]
				if !peer.held || !peer.active || !peer.requiresAdmission {
					t.Fatal("test did not establish an unqualified ACTIVE replacement")
				}
				requireAdmission(t, a, target, now, false)
			})
		}
	}
}

func TestStoppedSwapKeepActiveCannotBypassEvidence(t *testing.T) {
	for _, keep := range []string{"off", "manual", "aux"} {
		for target := 0; target < 2; target++ {
			for _, failure := range []string{"missing-motion", "movement", "stale-motion", "voltage-mismatch", "own-off", "peer-off"} {
				t.Run(fmt.Sprintf("%s/slot%d/%s", keep, target, failure), func(t *testing.T) {
					a, now := stoppedSwapPolicy(t, true, true, keep, target, "tag")
					switch failure {
					case "missing-motion":
						a.invalidateMotion()
					case "movement":
						a.observeMotion(false, now.Add(time.Millisecond), now.Add(time.Millisecond))
						now = now.Add(time.Millisecond)
					case "stale-motion":
						now = now.Add(stoppedMotionMaxAge + time.Nanosecond)
					case "voltage-mismatch":
						a.slots[target].frame.Voltage += 5000
						for i := range a.slots[target].history {
							a.slots[target].history[i].data.Voltage += 5000
						}
					case "own-off":
						a.slots[target].held = true
						a.offAttempt(target, now)
					case "peer-off":
						a.offAttempt(1-target, now)
					}
					requireAdmission(t, a, target, now, false)
					if failure != "peer-off" {
						requireAdmission(t, a, 1-target, now, true)
					}
				})
			}
		}
	}
}

// Readings taken in a drive state do not carry over to parking: the vehicle
// change discards them, and after a short stand-still both packs are read again.
func requalifyAfterParking(t *testing.T, a *packAdmission, now time.Time) time.Time {
	t.Helper()
	a.setVehicleAt(VehicleStateParked, now)
	for i := range a.slots {
		if a.slots[i].history != nil {
			t.Fatal("vehicle change kept pre-park measurements")
		}
	}
	frames := [2]BMSData{a.slots[0].frame, a.slots[1].frame}
	at := now.Add(admissionStillTime + time.Second)
	a.sampling.pairedID = 0
	id, _ := a.beginRound(at)
	for i := range frames {
		if id == 0 || a.claimRound(i, at) != id {
			t.Fatal("fresh round not opened after the stand-still")
		}
		a.observeAcquisition(i, id, acquisition{start: at, end: at.Add(100 * time.Millisecond), complete: at.Add(200 * time.Millisecond), data: frames[i]})
	}
	return at.Add(200 * time.Millisecond)
}
