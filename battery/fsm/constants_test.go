package fsm

import (
	"testing"
	"time"
)

// Timing constants affect battery communication and polling behavior.
// These tests lock in the expected values to prevent accidental changes.
// If you need to change a value, update both the constant and this test intentionally.
func TestTimingConstants(t *testing.T) {
	cases := []struct {
		name     string
		got      time.Duration
		expected time.Duration
	}{
		{"timeCmd", timeCmd, 400 * time.Millisecond},
		{"timeReinit", timeReinit, 2 * time.Second},
		{"timeDeparture", timeDeparture, 500 * time.Millisecond},
		{"timeCheckReader", timeCheckReader, 10 * time.Second},
		{"timeCheckPresence", timeCheckPresence, 10 * time.Second},
		{"timeMaintPollInterval", timeMaintPollInterval, 30 * time.Minute},
	}

	for _, tc := range cases {
		if tc.got != tc.expected {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.expected)
		}
	}
}
