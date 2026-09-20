package fsm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type offTestActions struct {
	noopActions

	mu            sync.Mutex
	writeErrors   []error
	statusStates  []OffState
	statusErrors  []error
	writeTimes    []time.Time
	statusReads   int
	seatboxClosed bool
	// lastCmd models the reader's command timestamp so wait_last_cmd can be
	// exercised; commands records what the FSM asked the pack to do.
	lastCmd      time.Time
	commands     []BMSCommand
	commandTimes []time.Time
	openedTime   time.Duration
	sendOn       bool
	readStarted  chan struct{}
	readRelease  chan struct{}
}

func (a *offTestActions) GetSeatboxLockClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seatboxClosed
}

func (a *offTestActions) setSeatboxClosed(closed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seatboxClosed = closed
}

func (a *offTestActions) GetOpenedTime(bool, bool) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.openedTime != 0 {
		return a.openedTime
	}
	return time.Hour
}

func (a *offTestActions) ShouldSendOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sendOn
}

func (a *offTestActions) GetRemainingCmdTime() time.Duration {
	a.mu.Lock()
	last := a.lastCmd
	a.mu.Unlock()
	if last.IsZero() {
		return 0
	}
	if elapsed := time.Since(last); elapsed < timeCmd {
		return timeCmd - elapsed
	}
	return 0
}

func (a *offTestActions) WriteCommand(cmd BMSCommand) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commands = append(a.commands, cmd)
	a.commandTimes = append(a.commandTimes, time.Now())
	a.lastCmd = time.Now()
}

func (a *offTestActions) setLastCmdAgo(ago time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastCmd = time.Now().Add(-ago)
}

func (a *offTestActions) commandsSnapshot() ([]BMSCommand, []time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]BMSCommand(nil), a.commands...), append([]time.Time(nil), a.commandTimes...)
}
func (a *offTestActions) IsInactive() bool { return false }

func (a *offTestActions) WriteOffCommand() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.writeTimes = append(a.writeTimes, time.Now())
	if len(a.writeErrors) == 0 {
		return nil
	}
	err := a.writeErrors[0]
	a.writeErrors = a.writeErrors[1:]
	return err
}

func (a *offTestActions) ReadFreshOffState() (OffState, error) {
	a.mu.Lock()
	a.statusReads++
	state := OffStateActive
	if len(a.statusStates) > 0 {
		state = a.statusStates[0]
		a.statusStates = a.statusStates[1:]
	}
	var err error
	if len(a.statusErrors) > 0 {
		err = a.statusErrors[0]
		a.statusErrors = a.statusErrors[1:]
	}
	started := a.readStarted
	release := a.readRelease
	a.mu.Unlock()

	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	return state, err
}

func (a *offTestActions) snapshot() (writes []time.Time, statusReads int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Time(nil), a.writeTimes...), a.statusReads
}

func startOffTestMachine(t *testing.T, actions *offTestActions) (*StateMachine, context.CancelFunc) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := New(actions, log)
	ctx, cancel := context.WithCancel(context.Background())
	if err := sm.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	sm.SendEvent(EvInitComplete)
	waitFor(t, time.Second, func() bool { return sm.State() == StateWaitArrival }, "wait_arrival")
	sm.SendEvent(EvTagArrived)
	waitFor(t, time.Second, func() bool { return sm.State() == StateSendOff }, "first OFF attempt")
	return sm, cancel
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestOffFreshInactiveAdvances(t *testing.T) {
	actions := &offTestActions{statusStates: []OffState{OffStateInactive}}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()

	waitFor(t, time.Second, func() bool { return sm.State() == StateSendOpened }, "fresh inactive confirmation")
	writes, reads := actions.snapshot()
	if len(writes) != 1 || reads != 1 {
		t.Fatalf("writes=%d status reads=%d, want 1 each", len(writes), reads)
	}
}

func TestOffFreshActiveRetriesAfter400Milliseconds(t *testing.T) {
	actions := &offTestActions{statusStates: []OffState{OffStateActive, OffStateInactive}}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()

	waitFor(t, 2*time.Second, func() bool { return sm.State() == StateSendOpened }, "inactive after ACTIVE retry")
	writes, reads := actions.snapshot()
	if len(writes) != 2 || reads != 2 {
		t.Fatalf("writes=%d status reads=%d, want 2 each", len(writes), reads)
	}
	if elapsed := writes[1].Sub(writes[0]); elapsed < timeCmd {
		t.Fatalf("OFF retry spacing=%v, want at least %v", elapsed, timeCmd)
	}
}

func TestOffFailedStatusRejectsCachedIdle(t *testing.T) {
	actions := &offTestActions{
		statusStates: []OffState{OffStateInactive, OffStateInactive},
		statusErrors: []error{errors.New("unparsed status"), nil},
	}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()

	waitFor(t, 2*time.Second, func() bool { return sm.State() == StateSendOpened }, "retry after failed status")
	writes, reads := actions.snapshot()
	if len(writes) != 2 || reads != 2 {
		t.Fatalf("writes=%d status reads=%d, want failed sample rejected and retried", len(writes), reads)
	}
}

func TestOffWriteErrorStillReadsFreshStatusAndRetries(t *testing.T) {
	actions := &offTestActions{
		writeErrors:  []error{errors.New("write failed"), nil},
		statusStates: []OffState{OffStateActive, OffStateInactive},
	}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()

	waitFor(t, 2*time.Second, func() bool { return sm.State() == StateSendOpened }, "retry after write error")
	writes, reads := actions.snapshot()
	if len(writes) != 2 || reads != 2 {
		t.Fatalf("writes=%d status reads=%d, want fresh confirmation after both attempts", len(writes), reads)
	}
	if elapsed := writes[1].Sub(writes[0]); elapsed < timeCmd {
		t.Fatalf("write-error retry spacing=%v, want at least %v", elapsed, timeCmd)
	}
}

func TestOffSafetyTimeoutRearmsAttempt(t *testing.T) {
	// A read that outlives the state timeout models the re-arm path: the queued
	// retry moves the machine, so the read's own result can no longer advance it
	// and the OFF attempt is repeated. The drop-a-send case needs a full event
	// queue and is not exercised here.
	original := offConfirmTimeout
	offConfirmTimeout = 50 * time.Millisecond
	defer func() { offConfirmTimeout = original }()

	actions := &offTestActions{
		statusStates: []OffState{OffStateInactive, OffStateInactive},
		readStarted:  make(chan struct{}, 1),
		readRelease:  make(chan struct{}),
	}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()

	select {
	case <-actions.readStarted:
	case <-time.After(time.Second):
		t.Fatal("OFF status read did not start")
	}
	// Let the timeout fire well before the read is released.
	time.Sleep(4 * offConfirmTimeout)
	close(actions.readRelease)

	waitFor(t, 3*time.Second, func() bool { return sm.State() == StateSendOpened }, "confirmation after safety timeout")
	writes, reads := actions.snapshot()
	if reads != 2 {
		t.Fatalf("status reads=%d, want the OFF attempt re-armed once", reads)
	}
	if len(writes) != 2 {
		t.Fatalf("writes=%d, want one OFF write per attempt", len(writes))
	}
}

func TestOffInFlightReadInterruptionIgnoresStaleResult(t *testing.T) {
	actions := &offTestActions{
		statusStates: []OffState{OffStateInactive},
		readStarted:  make(chan struct{}, 1),
		readRelease:  make(chan struct{}),
	}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()

	select {
	case <-actions.readStarted:
	case <-time.After(time.Second):
		t.Fatal("OFF status read did not start")
	}
	// librefsm has one FIFO event loop. While OnEnter is blocked in the read,
	// recovery queues before the result that OnEnter sends after release.
	sm.SendEvent(EvReinit)
	close(actions.readRelease)

	waitFor(t, time.Second, func() bool { return sm.State() == StateNFCReaderOff }, "NFC recovery before stale result")
	time.Sleep(20 * time.Millisecond)
	if sm.State() != StateNFCReaderOff {
		t.Fatalf("stale OFF result changed state to %s", sm.State())
	}
	if _, reads := actions.snapshot(); reads != 1 {
		t.Fatalf("status reads=%d, want one completed in-flight read", reads)
	}
}

func TestOffUsesProduction400MillisecondInterval(t *testing.T) {
	if timeCmd != 400*time.Millisecond {
		t.Fatalf("timeCmd=%v, want 400ms", timeCmd)
	}
}
