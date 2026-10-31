package battery

import (
	"testing"
	"time"
)

func TestEnergySampleRequiresAcquisition(t *testing.T) {
	r := &BatteryReader{}
	if at, valid := r.energySampleFields(); at != 0 || valid {
		t.Fatal(at, valid)
	}
	r.data = BMSData{Present: true, SampleTime: time.Unix(1700000000, 0), SampleValid: true}
	at, valid := r.energySampleFields()
	if at != 1700000000000 || !valid {
		t.Fatal(at, valid)
	}
	// A synthetic state change does not renew the measurement.
	r.data.State = BMSStateIdle
	if next, ok := r.energySampleFields(); next != at || !ok {
		t.Fatal(next, ok)
	}
	r.fresh.unconfirmed = true
	if _, ok := r.energySampleFields(); ok {
		t.Fatal("unconfirmed tag is usable")
	}
	r.fresh.unconfirmed = false
	r.data.SampleValid = false
	if _, ok := r.energySampleFields(); ok {
		t.Fatal("invalid acquisition is usable")
	}
	r.data.SampleValid = true
	r.data.Present = false
	if _, ok := r.energySampleFields(); ok {
		t.Fatal("absent pack is usable")
	}
	r.data = BMSData{}
	if at, valid := r.energySampleFields(); at != 0 || valid {
		t.Fatal(at, valid)
	}
}
