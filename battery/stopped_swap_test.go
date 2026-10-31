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
	if !a.motion.enabled {
		t.Fatal("incorrect combined experimental settings")
	}
	a.setVehicle(VehicleStateParked)
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
	a.setVehicle(VehicleStateReadyToDrive)
	now := admissionTestTime.Add(time.Second)
	a.observeMotion(true, now.Add(-100*time.Millisecond), now.Add(-100*time.Millisecond))
	window := func(start time.Time, checkEarly bool) time.Time {
		t.Helper()
		var end time.Time
		for tick := 0; tick < 3; tick++ {
			at := start.Add(time.Duration(tick) * admissionPollInterval)
			feedStoppedMotion(a, at)
			id, _ := a.beginRound(at)
			for i := range frames {
				if id == 0 || a.claimRound(i, at) != id {
					t.Fatal("pair not claimed")
				}
				data := frames[i]
				data.Voltage += uint(tick % 2)
				a.observeAcquisition(i, id, acquisition{start: at, end: at.Add(100 * time.Millisecond), complete: at.Add(200 * time.Millisecond), data: data})
			}
			end = at.Add(200 * time.Millisecond)
			feedStoppedMotion(a, end)
			if checkEarly && tick < 2 {
				requireAdmission(t, a, target, end, false)
			}
		}
		return end
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
