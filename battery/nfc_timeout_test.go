package battery

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"battery-service/battery/fsm"
	"github.com/librescoot/pn7150"
)

func TestNFCErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name              string
		err               error
		recover, departed bool
	}{
		{"notification timeout", hal.NewTagDepartedError("timeout waiting for notification 0x0105"), true, false},
		{"credit timeout", hal.NewTagDepartedError("timeout waiting for response after credit notification"), true, false},
		{"wrapped timeout", fmt.Errorf("read: %w", hal.NewTagDepartedError("timeout waiting for notification 0x0105")), true, false},
		{"RF deactivation", hal.NewTagDepartedError("RF deactivated during read"), false, true},
		{"wrapped departure", fmt.Errorf("read: %w", hal.NewTagDepartedError("RF deactivated during read")), false, true},
		{"I2C timeout", hal.NewI2CTimeoutError("poll timeout"), true, false},
		{"untyped timeout", errors.New("timeout waiting for notification 0x0105"), false, false},
		{"nil", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHALError(tc.err); got != tc.recover {
				t.Errorf("isHALError = %t, want %t", got, tc.recover)
			}
			if got := isTagDepartedError(tc.err); got != tc.departed {
				t.Errorf("isTagDepartedError = %t, want %t", got, tc.departed)
			}
		})
	}
}

func TestResponseTimeoutReinitializesWithoutPublishingRemoval(t *testing.T) {
	r, _ := newDriveLatchReader(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.fsm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.fsm.SendEvent(fsm.EvInitComplete)
	waitDriveLatchState(t, r, fsm.StateWaitArrival)
	r.fsm.SendEvent(fsm.EvTagArrived)
	waitDriveLatchState(t, r, fsm.StateWaitUpdate)
	r.previousTagPresent = true
	r.currentTagUID = []byte{1, 2, 3}
	r.data.Voltage = 52000

	r.handleNFCError(hal.NewTagDepartedError("timeout waiting for notification 0x0105"))
	waitDriveLatchState(t, r, fsm.StateNFCReaderOff)
	if !r.previousTagPresent || !r.data.Present || r.data.Voltage != 52000 || len(r.currentTagUID) == 0 {
		t.Fatal("timeout erased pack data as if physical removal had been confirmed")
	}
	if r.commFailureCount != 1 {
		t.Fatalf("communication failures = %d, want 1", r.commFailureCount)
	}
	// No external event is needed to power the reader back on and discover.
	waitDriveLatchState(t, r, fsm.StateWaitArrival)
}
