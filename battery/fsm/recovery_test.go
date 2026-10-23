package fsm

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestAbsentReaderWatchdogReinitializes(t *testing.T) {
	sm := New(noopActions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	sm.SendEvent(EvInitComplete)
	waitFor(t, time.Second, func() bool { return sm.State() == StateWaitArrival }, "discovery")
	sm.SendEvent(EvDepartureTimeout)
	waitFor(t, time.Second, func() bool { return sm.State() == StateTagAbsent }, "absent")
	// Advance the ten-second watchdog without a wall-clock sleep.
	sm.SendEvent(EvCheckReaderTimeout)
	waitFor(t, time.Second, func() bool { return sm.State() == StateNFCReaderOff }, "watchdog recovery")
	waitFor(t, 3*time.Second, func() bool { return sm.State() == StateWaitArrival }, "automatic rediscovery")
}

func TestGenuineDepartureDoesNotWaitForWatchdog(t *testing.T) {
	sm := New(noopActions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	sm.SendEvent(EvInitComplete)
	waitFor(t, time.Second, func() bool { return sm.State() == StateWaitArrival }, "discovery")
	sm.SendEvent(EvTagArrived)
	waitFor(t, time.Second, func() bool { return sm.IsInState(StateTagPresent) }, "present")
	sm.SendEvent(EvTagDeparted)
	waitFor(t, time.Second, func() bool { return sm.IsInState(StateDiscoverTag) }, "departure discovery")
}
