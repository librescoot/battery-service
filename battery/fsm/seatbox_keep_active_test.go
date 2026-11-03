package fsm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/librescoot/librefsm"
)

type keepActiveTestActions struct {
	*offTestActions
	keepActive bool
	inactive   bool
	manualTime bool
}

func (a *keepActiveTestActions) ShouldKeepActiveOnSeatboxOpen() bool { return a.keepActive }
func (a *keepActiveTestActions) IsInactive() bool                    { return a.inactive }
func (a *keepActiveTestActions) GetRemainingCmdTime() time.Duration {
	if a.manualTime {
		return time.Hour
	}
	return a.offTestActions.GetRemainingCmdTime()
}

func TestSeatboxKeepActiveAcrossReentry(t *testing.T) {
	for _, keepActive := range []bool{false, true} {
		for _, event := range []string{"restart", "close-reopen"} {
			t.Run(fmt.Sprintf("keep=%t/%s", keepActive, event), func(t *testing.T) {
				actions := &keepActiveTestActions{
					offTestActions: &offTestActions{seatboxClosed: true, sendOn: true},
					keepActive:     keepActive,
				}
				sm := New(actions, slog.New(slog.NewTextHandler(io.Discard, nil)))
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				if err := sm.Start(ctx); err != nil {
					t.Fatal(err)
				}
				sm.SendEvent(EvInitComplete)
				waitFor(t, time.Second, func() bool { return sm.State() == StateWaitArrival }, "wait_arrival")
				sm.SendEvent(EvTagArrived)
				waitFor(t, 2*time.Second, func() bool { return sm.State() == StateWaitUpdate }, "initial heartbeat")
				before, _ := actions.commandsSnapshot()

				switch event {
				case "restart":
					actions.setSeatboxClosed(false)
					sm.SendEvent(EvRestart)
				case "close-reopen":
					// A close schedules a command-spacing wait. The sensor can
					// report open again before that wait completes.
					actions.setLastCmdAgo(0)
					sm.SendEvent(EvSeatboxClosed)
					waitFor(t, time.Second, func() bool { return sm.State() == StateWaitLastCmd }, "close command wait")
					actions.setSeatboxClosed(false)
					sm.SendEvent(EvSeatboxOpened)
				}

				waitFor(t, 2*time.Second, func() bool {
					commands, _ := actions.commandsSnapshot()
					writes, _ := actions.snapshot()
					return len(commands) > len(before) || len(writes) > 0
				}, "next command after reentry")
				writes, _ := actions.snapshot()
				if !keepActive {
					if len(writes) == 0 {
						t.Fatal("disabled keep-active did not command OFF")
					}
					return
				}
				if len(writes) != 0 {
					t.Fatal("keep-active commanded OFF to a running battery")
				}
				waitFor(t, 2*time.Second, func() bool { return sm.State() == StateWaitUpdate }, "resumed heartbeat")
				commands, _ := actions.commandsSnapshot()
				if len(commands) != len(before)+2 || commands[len(before)] != BMSCmdSeatboxClosed || commands[len(before)+1] != BMSCmdOn {
					t.Fatalf("commands after reentry = %v, want CLOSED then ON", commands[len(before):])
				}
			})
		}
	}
}

func TestSeatboxReentryCommandPhaseMatrix(t *testing.T) {
	for _, keep := range []bool{false, true} {
		for _, sendOn := range []bool{false, true} {
			for _, inactive := range []bool{false, true} {
				for _, inserted := range []bool{false, true} {
					for _, phase := range []State{StateSendClosed, StateSendOnOff, StateWaitUpdate, StateSendInsertedClosed} {
						for _, action := range []string{"open", "restart", "close-reopen"} {
							t.Run(fmt.Sprintf("keep=%t/ON=%t/inactive=%t/inserted=%t/%s/%s", keep, sendOn, inactive, inserted, phase, action), func(t *testing.T) {
								a := &keepActiveTestActions{
									offTestActions: &offTestActions{seatboxClosed: true, sendOn: sendOn},
									keepActive:     keep, inactive: inactive, manualTime: true,
								}
								sm := New(a, slog.New(slog.NewTextHandler(io.Discard, nil)))
								sm.data.justInserted = inserted
								ctx, cancel := context.WithCancel(context.Background())
								t.Cleanup(cancel)
								if err := sm.Start(ctx); err != nil {
									t.Fatal(err)
								}
								t.Cleanup(func() { _ = sm.machine.Stop() })
								// Seed each command boundary independently; timer events
								// are advanced synchronously, without shortening production delays.
								if err := sm.machine.SetState(phase); err != nil {
									t.Fatal(err)
								}
								send := func(id librefsm.EventID) {
									t.Helper()
									if err := sm.machine.SendSync(librefsm.Event{ID: id}); err != nil {
										t.Fatal(err)
									}
								}
								before, _ := a.commandsSnapshot()
								if action == "close-reopen" {
									send(EvSeatboxClosed)
									if sm.State() != StateWaitLastCmd {
										t.Fatalf("close state = %s, want command-spacing wait", sm.State())
									}
								}
								a.setSeatboxClosed(false)
								if action == "restart" {
									send(EvRestart)
								} else {
									send(EvSeatboxOpened)
								}
								if action != "open" {
									send(EvLastCmdTimeout)
								}
								want := StateSendOff
								switch {
								case action == "open" && keep:
									want = phase
								case keep && !inactive:
									want = StateSendClosed
								case inserted && inactive:
									want = StateSendOpened
								}
								if sm.State() != want {
									t.Fatalf("state = %s, want %s", sm.State(), want)
								}
								writes, _ := a.snapshot()
								if (len(writes) != 0) != (want == StateSendOff) {
									t.Fatalf("OFF attempts = %d in state %s", len(writes), want)
								}
								if want == StateSendClosed && action != "open" {
									send(EvClosedTimeout)
									commands, _ := a.commandsSnapshot()
									wantCommand := BMSCmdOff
									if sendOn {
										wantCommand = BMSCmdOn
									}
									if len(commands) != len(before)+2 || commands[len(before)] != BMSCmdSeatboxClosed || commands[len(before)+1] != wantCommand {
										t.Fatalf("commands = %v, want CLOSED then %s", commands[len(before):], wantCommand)
									}
								} else if action == "open" && keep {
									commands, _ := a.commandsSnapshot()
									if len(commands) != len(before) {
										t.Fatal("opening interrupted the protected command phase")
									}
								}
							})
						}
					}
				}
			}
		}
	}
}
