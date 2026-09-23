package battery

import "testing"

func TestAuxLowKeepActiveLatch(t *testing.T) {
	const (
		enter = uint64(11500)
		exit  = uint64(12000)
	)

	cases := []struct {
		name        string
		was         bool
		haveHistory bool
		mv          uint64
		want        bool
	}{
		// The first valid reading after start has no latch to resume, so it is
		// judged against exit: anything short of a healthy aux keeps the pack
		// awake, including the marginal Enter..Exit band.
		{"first reading, well below enter", false, false, 11000, true},
		{"first reading, at enter", false, false, 11500, true},
		{"first reading, between enter and exit", false, false, 11700, true},
		{"first reading, at exit", false, false, 12000, false},
		{"first reading, above exit", false, false, 12500, false},

		// Once a reading has been applied the latch holds the usual hysteresis.
		{"engaged, stays in band", true, true, 11700, true},
		{"engaged, drops further", true, true, 11000, true},
		{"engaged, reaches exit", true, true, 12000, false},
		{"engaged, above exit", true, true, 12500, false},
		{"disengaged, stays in band", false, true, 11700, false},
		{"disengaged, at enter", false, true, 11500, false},
		{"disengaged, below enter", false, true, 11000, true},
		{"disengaged, above exit", false, true, 12500, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := auxLowKeepActiveLatch(tc.was, tc.haveHistory, tc.mv, enter, exit)
			if got != tc.want {
				t.Errorf("auxLowKeepActiveLatch(was=%t, haveHistory=%t, mv=%d, enter=%d, exit=%d) = %t, want %t",
					tc.was, tc.haveHistory, tc.mv, enter, exit, got, tc.want)
			}
		})
	}
}
