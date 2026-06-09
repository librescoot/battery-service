package battery

import "testing"

func TestNextLatchedSeatboxClosed(t *testing.T) {
	const (
		closed = true
		open   = false
	)
	cases := []struct {
		name         string
		vehicleState VehicleState
		currentLatch bool
		rawClosed    bool
		want         bool
	}{
		{"parked, opens", VehicleStateParked, closed, open, open},
		{"parked, closes", VehicleStateParked, open, closed, closed},
		{"standby, opens", VehicleStateStandby, closed, open, open},
		// While ready-to-drive an opening report is ignored so a latch-sensor
		// bounce can't power the active battery off mid-ride.
		{"ready-to-drive, latched closed, bounces open -> stays closed", VehicleStateReadyToDrive, closed, open, closed},
		{"ready-to-drive, stays closed", VehicleStateReadyToDrive, closed, closed, closed},
		// Closing during ready-to-drive still latches closed.
		{"ready-to-drive, latched open, closes -> closed", VehicleStateReadyToDrive, open, closed, closed},
		{"ready-to-drive, latched open, stays open", VehicleStateReadyToDrive, open, open, open},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextLatchedSeatboxClosed(tc.vehicleState, tc.currentLatch, tc.rawClosed)
			if got != tc.want {
				t.Errorf("nextLatchedSeatboxClosed(%q, latch=%t, raw=%t) = %t, want %t",
					tc.vehicleState, tc.currentLatch, tc.rawClosed, got, tc.want)
			}
		})
	}
}
