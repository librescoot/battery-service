package fsm

import (
	"testing"
	"time"
)

// EvSeatboxClosed is declared on the tag_present parent, so it must release
// every state where the FSM can rest with the pack powered down while the
// seatbox has closed again, and it must keep the 400ms command spacing.

func TestSeatboxClosedLeavesOffEpisode(t *testing.T) {
	actions := &offTestActions{statusStates: []OffState{OffStateActive}}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()

	actions.setSeatboxClosed(true)
	sm.SendEvent(EvSeatboxClosed)
	waitFor(t, time.Second, func() bool {
		return sm.IsInState(StateHeartbeat) && !sm.IsInState(StateSendOff)
	}, "close leaves the OFF episode")
}

func TestSeatboxClosedLeavesOpenLoop(t *testing.T) {
	actions := &offTestActions{statusStates: []OffState{OffStateInactive}}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()
	waitFor(t, 2*time.Second, func() bool { return sm.State() == StateSendOpened }, "reached send_opened")

	actions.setSeatboxClosed(true)
	sm.SendEvent(EvSeatboxClosed)
	waitFor(t, 2*time.Second, func() bool {
		return sm.IsInState(StateHeartbeat) && !sm.IsInState(StateSendOpened)
	}, "close leaves the seatbox-open loop")
}

// Reaching an inserted-open state needs a short opened timeout, otherwise the
// fake holds the FSM in send_opened for an hour.
func TestSeatboxClosedLeavesInsertedOpen(t *testing.T) {
	actions := &offTestActions{statusStates: []OffState{OffStateInactive}, openedTime: 20 * time.Millisecond}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()
	waitFor(t, 3*time.Second, func() bool { return sm.State() == StateSendInsertedOpen }, "reached send_inserted_open")

	actions.setSeatboxClosed(true)
	sm.SendEvent(EvSeatboxClosed)
	waitFor(t, 2*time.Second, func() bool {
		return sm.IsInState(StateHeartbeat) && !sm.IsInState(StateSendInsertedOpen)
	}, "close leaves send_inserted_open")
}

func TestSeatboxClosedWaitsOutCommandInterval(t *testing.T) {
	actions := &offTestActions{statusStates: []OffState{OffStateInactive}, sendOn: true}
	sm, cancel := startOffTestMachine(t, actions)
	defer cancel()
	waitFor(t, 2*time.Second, func() bool { return sm.State() == StateSendOpened }, "reached send_opened")

	// Pretend a command was written just now: the close must not jump the queue.
	actions.setLastCmdAgo(0)
	closedAt := time.Now()
	actions.setSeatboxClosed(true)
	sm.SendEvent(EvSeatboxClosed)

	waitFor(t, 3*time.Second, func() bool {
		commands, _ := actions.commandsSnapshot()
		for _, cmd := range commands {
			if cmd == BMSCmdSeatboxClosed {
				return true
			}
		}
		return false
	}, "seatbox closed command")

	commands, times := actions.commandsSnapshot()
	closedIndex := -1
	for i, cmd := range commands {
		if cmd == BMSCmdSeatboxClosed {
			closedIndex = i
			break
		}
	}
	if closedIndex < 0 {
		t.Fatal("seatbox closed command not recorded")
	}
	if gap := times[closedIndex].Sub(closedAt); gap < timeCmd {
		t.Fatalf("seatbox closed written after %v, want at least %v", gap, timeCmd)
	}

	// The close path must reach the ON write, still spaced from the closed command.
	waitFor(t, 3*time.Second, func() bool {
		commands, _ := actions.commandsSnapshot()
		return len(commands) > 0 && commands[len(commands)-1] == BMSCmdOn
	}, "ON command after close")
	_, times = actions.commandsSnapshot()
	last := times[len(times)-1]
	if gap := last.Sub(times[closedIndex]); gap < timeCmd {
		t.Fatalf("ON written %v after seatbox closed, want at least %v", gap, timeCmd)
	}
}
