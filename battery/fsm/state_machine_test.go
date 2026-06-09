package fsm

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// noopActions implements BatteryActions with no-op stubs so the FSM can be
// constructed in tests without dragging in Redis, NFC HAL, etc.
type noopActions struct{}

func (noopActions) TakeInhibitor()                 {}
func (noopActions) ReleaseInhibitor()              {}
func (noopActions) StartDiscovery() error          { return nil }
func (noopActions) StopDiscovery()                 {}
func (noopActions) SelectTag()                     {}
func (noopActions) PollForTagArrival() bool        { return true }
func (noopActions) Initialize() error              { return nil }
func (noopActions) Deinitialize()                  {}
func (noopActions) ReadStatus() error              { return nil }
func (noopActions) SendCheckPresenceReady()        {}
func (noopActions) WriteCommand(cmd BMSCommand)    {}
func (noopActions) GetEnabled() bool               { return false }
func (noopActions) GetSeatboxLockClosed() bool     { return true }
func (noopActions) GetVehicleActive() bool         { return false }
func (noopActions) CheckStateCorrect() bool        { return true }
func (noopActions) GetRemainingCmdTime() time.Duration {
	return 0
}
func (noopActions) GetOpenedTime(bool, bool) time.Duration { return 0 }
func (noopActions) GetInsertedTime(bool) time.Duration     { return 0 }
func (noopActions) GetHeartbeatInterval() time.Duration  { return time.Second }
func (noopActions) IsInactive() bool                     { return true }
func (noopActions) ZeroRetryCounters()                   {}
func (noopActions) StopHeartbeatTimer()                  {}
func (noopActions) ShouldKeepActiveOnSeatboxOpen() bool  { return false }
func (noopActions) StartHeartbeatTimer()                 {}
func (noopActions) ClearHeartbeatTimer()                 {}
func (noopActions) StopTimerIfBatteryEmpty()             {}
func (noopActions) IsRoleInactive() bool                 { return false }

// TestStartIsSynchronous locks in the invariant that fixes bean librescoot-5u06:
// after StateMachine.Start returns, the machine MUST be in StateInit. Callers
// (e.g. reader.checkInitComplete) rely on this — the previous async Run()
// raced and allowed checkInitComplete to evaluate State() before the FSM had
// even entered its initial state, swallowing the EvInitComplete event.
func TestStartIsSynchronous(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := New(noopActions{}, log)
	if sm == nil {
		t.Fatal("New returned nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := sm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := sm.State(); got != StateInit {
		t.Errorf("State immediately after Start = %q, want %q", got, StateInit)
	}
}

// TestEvInitCompleteFromStateInit confirms that the FSM transitions out of
// StateInit when EvInitComplete is received — i.e. the reader's
// checkInitComplete path will work as soon as Start has returned.
func TestEvInitCompleteFromStateInit(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := New(noopActions{}, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := sm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sm.SendEvent(EvInitComplete)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sm.State() != StateInit {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("FSM still in StateInit after EvInitComplete, want any other state")
}
