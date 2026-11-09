package battery

import (
	"fmt"
	"testing"
	"time"
)

func storagePeerPolicy() *packAdmission {
	a := newPackAdmission(&BatteryConfiguration{Readers: []BatteryReaderConfig{
		{Index: 0, Enabled: true, Role: BatteryRoleActive},
		{Index: 1, Enabled: true, Role: BatteryRoleInactive},
	}})
	a.setVehicleAt(VehicleStateParked, admissionTestTime.Add(-time.Hour))
	return a
}

func TestAdmissionDoesNotWaitForStorageOnlyPeer(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, peer := range []string{"undiscovered", "zero data", "stale idle", "unconfirmed idle"} {
			t.Run(fmt.Sprintf("strict=%t/%s", strict, peer), func(t *testing.T) {
				a := storagePeerPolicy()
				a.conservative, a.sampling.enabled = strict, true
				switch peer {
				case "zero data":
					a.tag(1, []byte{1})
					a.failAcquisition(1, 0, acquisition{})
				case "stale idle":
					observeWritten(a, 1, admissionFrame(50000, BMSStateIdle), admissionTestTime.Add(-2*admissionMaxAge))
				case "unconfirmed idle":
					a.observe(1, admissionFrame(50000, BMSStateIdle), admissionTestTime)
				}
				requireAdmission(t, a, 0, admissionTestTime, false)
				observeWritten(a, 0, admissionFrame(56000, BMSStateIdle), admissionTestTime)
				requireAdmission(t, a, 0, admissionTestTime, true)
				requireAdmission(t, a, 1, admissionTestTime, false)
			})
		}
	}
}

func TestAdmissionRetainsPeerProtectionAfterRoleChange(t *testing.T) {
	a := storagePeerPolicy()
	a.setRole(1, true)
	a.setRole(1, false)
	a.tag(1, []byte{1})
	a.failAcquisition(1, 0, acquisition{})
	observeWritten(a, 0, admissionFrame(56000, BMSStateIdle), admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, false)
	a.observeAbsent(1, admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, true)
}

func TestAdmissionStoragePeerActiveEvidenceCannotBeIgnored(t *testing.T) {
	for _, change := range []string{"none", "failed read", "changed tag", "changed serial"} {
		t.Run(change, func(t *testing.T) {
			a := storagePeerPolicy()
			a.tag(1, []byte{1})
			data := admissionFrame(50000, BMSStateActive)
			data.SerialNumber = "pack-a"
			// Even a first, unconfirmed ACTIVE frame makes this a possible supply.
			a.observe(1, data, admissionTestTime)
			switch change {
			case "failed read":
				a.failAcquisition(1, 0, acquisition{})
			case "changed tag":
				a.tag(1, []byte{2})
			case "changed serial":
				data.State, data.SerialNumber = BMSStateIdle, "pack-b"
				a.observe(1, data, admissionTestTime.Add(time.Second))
			}
			observeWritten(a, 0, admissionFrame(56000, BMSStateIdle), admissionTestTime.Add(time.Second))
			requireAdmission(t, a, 0, admissionTestTime.Add(time.Second), false)
		})
	}
}
