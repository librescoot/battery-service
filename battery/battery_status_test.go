package battery

import "testing"

func TestUpdateTemperatureState(t *testing.T) {
	cases := []struct {
		name string
		temp [4]int
		want BMSTemperatureState
	}{
		{"all ideal", [4]int{20, 21, 22, 23}, BMSTemperatureStateIdeal},
		{"just inside both limits", [4]int{3, 42, 20, 20}, BMSTemperatureStateIdeal},
		{"single sensor at cold limit", [4]int{20, 2, 25, 30}, BMSTemperatureStateCold},
		{"single sensor below cold limit", [4]int{-5, 25, 25, 25}, BMSTemperatureStateCold},
		{"single sensor at hot limit", [4]int{20, 20, 43, 20}, BMSTemperatureStateHot},
		{"single sensor above hot limit", [4]int{50, 20, 20, 20}, BMSTemperatureStateHot},
		{"first out-of-range sensor wins: hot before cold", [4]int{50, -5, 20, 20}, BMSTemperatureStateHot},
		{"first out-of-range sensor wins: cold before hot", [4]int{-5, 20, 50, 20}, BMSTemperatureStateCold},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BatteryReader{}
			r.data.Temperature = tc.temp
			r.updateTemperatureState()
			if r.data.TemperatureState != tc.want {
				t.Errorf("temp=%v: got state %d, want %d", tc.temp, r.data.TemperatureState, tc.want)
			}
		})
	}
}
