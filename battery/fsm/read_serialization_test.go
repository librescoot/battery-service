package fsm

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type serializedReadActions struct {
	noopActions
	reads    atomic.Int32
	entered  chan struct{}
	release  chan struct{}
	off      chan struct{}
	presence func() bool
	badQuery atomic.Bool
}

func (a *serializedReadActions) ReadStatus() error {
	if !a.presence() {
		a.badQuery.Store(true)
	}
	if a.reads.Add(1) == 2 {
		close(a.entered)
		<-a.release
	}
	return nil
}

func (a *serializedReadActions) WriteOffCommand() error {
	select {
	case a.off <- struct{}{}:
	default:
	}
	return nil
}

func TestCommandStatusReadSerializesWithSeatboxTransition(t *testing.T) {
	a := &serializedReadActions{
		entered: make(chan struct{}), release: make(chan struct{}), off: make(chan struct{}, 1),
	}
	sm := New(a, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Presence queries from within an action must not recursively take the
	// machine lock: fault callbacks use the same query under their own lock.
	a.presence = func() bool { return sm.IsInState(StateTagPresent) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var release sync.Once
	defer release.Do(func() { close(a.release) })
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if sm.IsInState(StateTagPresent) {
		t.Fatal("init reports a present tag")
	}
	sm.SendEvent(EvInitComplete)
	waitFor(t, time.Second, func() bool { return sm.State() == StateWaitArrival }, "discovery")
	sm.SendEvent(EvTagArrived)
	select {
	case <-a.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("command status read did not begin")
	}
	sm.SendEvent(EvSeatboxOpened)
	select {
	case <-a.off:
		t.Fatal("seatbox transition ran concurrently with the status read")
	case <-time.After(100 * time.Millisecond):
	}
	release.Do(func() { close(a.release) })
	select {
	case <-a.off:
	case <-time.After(time.Second):
		t.Fatal("queued seatbox transition did not run after the read")
	}
	if a.badQuery.Load() {
		t.Fatal("a tag-present action observed an absent hierarchy")
	}
	sm.SendEvent(EvTagDeparted)
	waitFor(t, time.Second, func() bool { return !sm.IsInState(StateTagPresent) }, "presence mirror cleared on departure")
}
