package battery

import (
	"fmt"
	"testing"
	"time"
)

func keepActiveAdmissionService(t *testing.T, conservative bool) (*Service, map[string]string) {
	t.Helper()
	values := map[string]string{
		"vehicle/seatbox:lock":                              "closed",
		"settings/experimental.battery.dual-pack-admission": fmt.Sprint(conservative),
	}
	s, _ := admissionSettingService(t, values)
	// Both readers are active, as with the dual-battery setting enabled.
	s.batteryConfig.Readers[1].Role = BatteryRoleActive
	s.loadAdmissionSetting()
	s.admission.setVehicleAt(VehicleStateParked, admissionTestTime.Add(-time.Hour))
	for i, reader := range s.readers {
		reader.seatboxLockChan = make(chan bool, 1)
		s.admission.tag(i, []byte{byte(i + 1)})
		data := admissionFrame(55000, BMSStateActive)
		data.SerialNumber = fmt.Sprintf("original-%d", i)
		data.Current = 2500
		observeWritten(s.admission, i, data, admissionTestTime)
	}
	s.handleSeatboxUpdate()
	return s, values
}

func TestSeatboxKeepActivePreservesOnlyEstablishedIdentities(t *testing.T) {
	for _, conservative := range []bool{false, true} {
		for _, mode := range []string{"disabled", "manual", "aux", "both"} {
			t.Run(fmt.Sprintf("conservative=%t/%s", conservative, mode), func(t *testing.T) {
				s, values := keepActiveAdmissionService(t, conservative)
				s.config.KeepActiveOnSeatboxOpen.Store(mode == "manual" || mode == "both")
				s.config.AuxLowKeepActive.Store(mode == "aux" || mode == "both")
				a := s.admission
				for _, state := range []string{"open", "closed"} {
					values["vehicle/seatbox:lock"] = state
					s.handleSeatboxUpdate()
					for i := range a.slots {
						if a.slots[i].valid || a.slots[i].observedRound != 0 || len(a.slots[i].history) != 0 {
							t.Fatal("keep-active retained stale measurement evidence")
						}
						requireAdmission(t, a, i, admissionTestTime.Add(time.Second), true)
					}
				}
				if mode == "disabled" {
					return
				}
				a.tag(1, []byte{9})
				replacement := admissionFrame(50000, BMSStateActive)
				replacement.SerialNumber = "replacement"
				observeWritten(a, 1, replacement, admissionTestTime.Add(2*time.Second))
				requireAdmission(t, a, 1, admissionTestTime.Add(2*time.Second), false)
				requireAdmission(t, a, 0, admissionTestTime.Add(2*time.Second), true)
			})
		}
	}
}

func TestSeatboxDoesNotRevokeRunningPermissionWhenParking(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			s, values := keepActiveAdmissionService(t, true)
			a := s.admission
			a.setVehicleAt(VehicleStateReadyToDrive, admissionTestTime.Add(-time.Hour))
			values["vehicle/seatbox:lock"] = "open"
			s.handleSeatboxUpdate()
			for i := range a.slots {
				requireAdmission(t, a, i, admissionTestTime.Add(time.Second), true)
			}
			// The AUX override can change without another seatbox notification.
			s.config.AuxLowKeepActive.Store(enabled)
			a.setVehicleAt(VehicleStateParked, admissionTestTime.Add(-time.Hour))
			for i := range a.slots {
				if a.slots[i].valid {
					t.Fatal("parking reused pre-seatbox measurement evidence")
				}
				requireAdmission(t, a, i, admissionTestTime.Add(2*time.Second), true)
			}
		})
	}
}

func TestSeatboxKeepActiveCannotRestoreReplacementPermission(t *testing.T) {
	for _, identity := range []string{"tag", "serial"} {
		t.Run(identity, func(t *testing.T) {
			s, values := keepActiveAdmissionService(t, true)
			s.config.KeepActiveOnSeatboxOpen.Store(true)
			a := s.admission
			values["vehicle/seatbox:lock"] = "open"
			s.handleSeatboxUpdate()
			if identity == "tag" {
				a.tag(1, []byte{9})
			}
			replacement := admissionFrame(50000, BMSStateActive)
			replacement.SerialNumber = "replacement"
			now := admissionTestTime.Add(2 * time.Second)
			observeWritten(a, 1, replacement, now)
			values["vehicle/seatbox:lock"] = "closed"
			s.handleSeatboxUpdate()
			observeWritten(a, 1, replacement, now.Add(time.Second))
			requireAdmission(t, a, 1, now.Add(time.Second), false)
			requireAdmission(t, a, 0, now.Add(time.Second), true)
			if !a.slots[1].requiresAdmission {
				t.Fatal("keep-active restored predecessor's permission")
			}
		})
	}
}

func TestSeatboxKeepActiveDoesNotPromotePreviouslyIdlePack(t *testing.T) {
	s, values := keepActiveAdmissionService(t, false)
	s.config.AuxLowKeepActive.Store(true)
	a := s.admission
	now := admissionTestTime.Add(time.Second)
	a.offAttempt(1, now)
	idle := a.slots[1].frame
	idle.State = BMSStateIdle
	observeWritten(a, 1, idle, now.Add(time.Second))
	if a.slots[1].held {
		t.Fatal("test setup did not confirm OFF")
	}
	values["vehicle/seatbox:lock"] = "open"
	s.handleSeatboxUpdate()
	idle.State = BMSStateActive
	observeWritten(a, 1, idle, now.Add(2*time.Second))
	requireAdmission(t, a, 1, now.Add(2*time.Second), false)
	requireAdmission(t, a, 0, now.Add(2*time.Second), true)
}

func TestSeatboxKeepActiveDoesNotCancelOffReservation(t *testing.T) {
	s, values := keepActiveAdmissionService(t, false)
	s.config.KeepActiveOnSeatboxOpen.Store(true)
	a := s.admission
	now := admissionTestTime.Add(time.Second)
	a.offAttempt(1, now)
	values["vehicle/seatbox:lock"] = "open"
	s.handleSeatboxUpdate()
	requireAdmission(t, a, 1, now, false)
	if !a.slots[1].offPending || !a.slots[1].held {
		t.Fatal("keep-active cancelled OFF reservation")
	}
}
