package battery

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"battery-service/battery/fsm"
)

type seatboxRecoveryActions struct {
	*driveLatchActions
	readFailures int
}

func (a *seatboxRecoveryActions) ReadStatus() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.readFailures > 0 {
		a.readFailures--
		return errors.New("NFC read failed")
	}
	return nil
}

func TestOpenSeatboxKeepsSupplyAcrossAutomaticNFCRecovery(t *testing.T) {
	for mode := 1; mode < 4; mode++ {
		for slot := 0; slot < 2; slot++ {
			t.Run(fmt.Sprintf("manual=%t/aux=%t/slot%d", mode&1 != 0, mode&2 != 0, slot), func(t *testing.T) {
				t.Parallel()
				r, base := newDriveLatchReader(slot)
				a := &seatboxRecoveryActions{driveLatchActions: base}
				r.service.config.KeepActiveOnSeatboxOpen.Store(mode&1 != 0)
				r.service.config.AuxLowKeepActive.Store(mode&2 != 0)
				r.fsm = fsm.New(a, r.logger)
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				if err := r.fsm.Start(ctx); err != nil {
					t.Fatal(err)
				}
				r.fsm.SendEvent(fsm.EvInitComplete)
				waitDriveLatchState(t, r, fsm.StateWaitArrival)
				r.fsm.SendEvent(fsm.EvTagArrived)
				waitDriveLatchState(t, r, fsm.StateWaitUpdate)
				r.handleSeatboxLockChange(false)

				// Settings stay constant. Three failed heartbeat reads must
				// trigger the production recovery path, not a synthetic restart.
				a.mu.Lock()
				a.readFailures = 3
				a.mu.Unlock()
				for cycle := 0; cycle < 3; cycle++ {
					r.fsm.SendEvent(fsm.EvHeartbeatTimeout)
					wantCommands := 4 + cycle*2
					if cycle == 2 {
						wantCommands += 2
					}
					deadline := time.Now().Add(3 * time.Second)
					for time.Now().Before(deadline) {
						if len(a.snapshot()) >= wantCommands && r.fsm.State() == fsm.StateWaitUpdate {
							break
						}
						time.Sleep(time.Millisecond)
					}
					commands := a.snapshot()
					if len(commands) != wantCommands || r.fsm.State() != fsm.StateWaitUpdate {
						t.Fatalf("cycle %d: state=%s commands=%v, want %d commands and resumed heartbeat", cycle, r.fsm.State(), commands, wantCommands)
					}
					for i, cmd := range commands {
						want := fsm.BMSCmdSeatboxClosed
						if i%2 == 1 {
							want = fsm.BMSCmdOn
						}
						if cmd != want {
							t.Fatalf("cycle %d: command %d=%s, want %s; trace=%v", cycle, i, cmd, want, commands)
						}
					}
				}
			})
		}
	}
}
