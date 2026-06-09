package battery

import (
	"testing"
	"time"
)

func TestGetOpenedTime(t *testing.T) {
	cases := []struct {
		name         string
		justInserted bool
		justOpened   bool
		state        BMSState
		want         time.Duration
	}{
		// Just inserted takes priority: light the LED ring as fast as possible.
		{"just inserted wins over just opened", true, true, BMSStateAsleep, BMSTimeCmd},
		{"just inserted", true, false, BMSStateActive, BMSTimeCmd},
		// First open of an already-present pack: mirror the wake-up time.
		{"first open, asleep", false, true, BMSStateAsleep, BMSTimeCmdFirstOpenedAsleep},
		{"first open, awake (active)", false, true, BMSStateActive, BMSTimeCmdFirstOpenedAwake},
		{"first open, awake (idle)", false, true, BMSStateIdle, BMSTimeCmdFirstOpenedAwake},
		// Steady-state maintenance loop runs at the slow rate.
		{"steady state", false, false, BMSStateActive, BMSTimeCmdSlow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BatteryReader{}
			r.data.State = tc.state
			if got := r.GetOpenedTime(tc.justInserted, tc.justOpened); got != tc.want {
				t.Errorf("GetOpenedTime(inserted=%t, opened=%t, state=%v) = %v, want %v",
					tc.justInserted, tc.justOpened, tc.state, got, tc.want)
			}
		})
	}
}

func TestGetInsertedTime(t *testing.T) {
	r := &BatteryReader{}
	if got := r.GetInsertedTime(true); got != BMSTimeCmd {
		t.Errorf("GetInsertedTime(justInserted=true) = %v, want %v", got, BMSTimeCmd)
	}
	if got := r.GetInsertedTime(false); got != BMSTimeCmdSlow {
		t.Errorf("GetInsertedTime(justInserted=false) = %v, want %v", got, BMSTimeCmdSlow)
	}
}
