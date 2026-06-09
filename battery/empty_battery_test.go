package battery

import (
	"io"
	"log/slog"
	"testing"
)

func TestIsBatteryEmpty(t *testing.T) {
	cases := []struct {
		name   string
		lowSOC bool
		charge uint
		want   bool
	}{
		{"low-soc flag set and zero charge", true, 0, true},
		{"low-soc flag set but charge remains", true, 5, false},
		{"zero charge without the bms flag", false, 0, false},
		{"healthy", false, 50, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BatteryReader{}
			r.data.LowSOC = tc.lowSOC
			r.data.Charge = tc.charge
			if got := r.isBatteryEmpty(); got != tc.want {
				t.Errorf("isBatteryEmpty(lowSOC=%t, charge=%d) = %t, want %t", tc.lowSOC, tc.charge, got, tc.want)
			}
		})
	}
}

func TestShouldSendOn(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		lowSOC  bool
		charge  uint
		want    bool
	}{
		{"enabled, healthy", true, false, 50, true},
		{"enabled but empty", true, true, 0, false},
		{"disabled, healthy", false, false, 50, false},
		{"disabled and empty", false, true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BatteryReader{}
			r.enabled = tc.enabled
			r.data.LowSOC = tc.lowSOC
			r.data.Charge = tc.charge
			if got := r.ShouldSendOn(); got != tc.want {
				t.Errorf("ShouldSendOn(enabled=%t, lowSOC=%t, charge=%d) = %t, want %t",
					tc.enabled, tc.lowSOC, tc.charge, got, tc.want)
			}
		})
	}
}

func TestCheckStateCorrect(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		lowSOC  bool
		charge  uint
		state   BMSState
		want    bool
	}{
		{"enabled healthy is active", true, false, 50, BMSStateActive, true},
		{"enabled healthy not active", true, false, 50, BMSStateIdle, false},
		{"enabled empty expects idle", true, true, 0, BMSStateIdle, true},
		{"enabled empty expects asleep", true, true, 0, BMSStateAsleep, true},
		{"enabled empty must not be active", true, true, 0, BMSStateActive, false},
		{"disabled is idle", false, false, 50, BMSStateIdle, true},
		{"disabled is asleep", false, false, 50, BMSStateAsleep, true},
		{"disabled not active", false, false, 50, BMSStateActive, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BatteryReader{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			r.enabled = tc.enabled
			r.data.LowSOC = tc.lowSOC
			r.data.Charge = tc.charge
			r.data.State = tc.state
			if got := r.CheckStateCorrect(); got != tc.want {
				t.Errorf("CheckStateCorrect(enabled=%t, empty=%t, state=%v) = %t, want %t",
					tc.enabled, tc.lowSOC && tc.charge == 0, tc.state, got, tc.want)
			}
		})
	}
}
