package fsm

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type admissionPollActions struct {
	noopActions
	mu    sync.Mutex
	trace []string
}

func (a *admissionPollActions) record(event string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.trace = append(a.trace, event)
}

func (a *admissionPollActions) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.trace...)
}

func (a *admissionPollActions) ReadStatus() error {
	a.record("read")
	return nil
}

func (a *admissionPollActions) ShouldSendOn() bool { return true }

func (a *admissionPollActions) WriteCommand(cmd BMSCommand) {
	a.record(cmd.String())
}

func startAdmissionPollMachine(t *testing.T) (*StateMachine, *admissionPollActions) {
	t.Helper()
	a := &admissionPollActions{}
	sm := New(a, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	sm.SendEvent(EvInitComplete)
	waitFor(t, time.Second, func() bool { return sm.State() == StateWaitArrival }, "discovery")
	sm.SendEvent(EvTagArrived)
	return sm, a
}

func TestAdmissionPollMaintainsSupplyBeforeReading(t *testing.T) {
	sm, a := startAdmissionPollMachine(t)
	waitFor(t, 2*time.Second, func() bool { return sm.State() == StateWaitUpdate }, "first heartbeat completed")
	before := len(a.snapshot())
	sm.SendEvent(EvAdmissionPoll)
	waitFor(t, 2*time.Second, func() bool {
		return len(a.snapshot()) >= before+3 && sm.State() == StateWaitUpdate
	}, "polled heartbeat completed")
	trace := a.snapshot()[before:]
	want := []string{BMSCmdSeatboxClosed.String(), BMSCmdOn.String(), "read"}
	if len(trace) != len(want) {
		t.Fatalf("poll trace = %v, want %v", trace, want)
	}
	for i := range want {
		if trace[i] != want[i] {
			t.Fatalf("poll trace = %v, want %v", trace, want)
		}
	}
}

func TestAdmissionPollDoesNotInterruptCommandSettling(t *testing.T) {
	sm, a := startAdmissionPollMachine(t)
	waitFor(t, time.Second, func() bool { return sm.State() == StateSendClosed }, "closed command")
	before := len(a.snapshot())
	for i := 0; i < 10; i++ {
		sm.SendEvent(EvAdmissionPoll)
	}
	time.Sleep(100 * time.Millisecond)
	if trace := a.snapshot(); len(trace) != before {
		t.Fatalf("poll interrupted command delay: %v", trace)
	}
	waitFor(t, 2*time.Second, func() bool { return sm.State() == StateWaitUpdate }, "command sequence completed")
}
