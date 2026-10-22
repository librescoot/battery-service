package battery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"battery-service/battery/fsm"
)

// Keep the real reader policy and FSM, replacing only hardware and timers.
// Heartbeats are advanced explicitly so sensor events occur between cycles.
type driveLatchActions struct {
	*BatteryReader
	mu       sync.Mutex
	commands []fsm.BMSCommand
}

func (*driveLatchActions) TakeInhibitor()                         {}
func (*driveLatchActions) ReleaseInhibitor()                      {}
func (*driveLatchActions) Initialize() error                      { return nil }
func (*driveLatchActions) Deinitialize()                          {}
func (*driveLatchActions) StartDiscovery() error                  { return nil }
func (*driveLatchActions) StopDiscovery()                         {}
func (*driveLatchActions) SelectTag()                             {}
func (*driveLatchActions) PollForTagArrival() bool                { return false }
func (*driveLatchActions) ReadStatus() error                      { return nil }
func (*driveLatchActions) StartHeartbeatTimer()                   {}
func (*driveLatchActions) ClearHeartbeatTimer()                   {}
func (*driveLatchActions) GetRemainingCmdTime() time.Duration     { return 0 }
func (*driveLatchActions) GetOpenedTime(bool, bool) time.Duration { return time.Hour }
func (*driveLatchActions) ReadFreshOffState() (fsm.OffState, error) {
	return fsm.OffStateInactive, nil
}
func (a *driveLatchActions) WriteCommand(cmd fsm.BMSCommand) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commands = append(a.commands, cmd)
}
func (a *driveLatchActions) WriteOffCommand() error {
	a.WriteCommand(fsm.BMSCmdOff)
	return nil
}
func (a *driveLatchActions) snapshot() []fsm.BMSCommand {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]fsm.BMSCommand(nil), a.commands...)
}

func newDriveLatchReader(index int) (*BatteryReader, *driveLatchActions) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &BatteryReader{
		index: index, role: BatteryRoleActive, enabled: true,
		vehicleState: VehicleStateParked, initCompleteSent: true,
		service: &Service{config: &ServiceConfig{}}, logger: logger,
		restartChan: make(chan struct{}, 1),
		data:        BMSData{Present: true, State: BMSStateActive, Charge: 80},
	}
	a := &driveLatchActions{BatteryReader: r}
	r.fsm = fsm.New(a, logger)
	r.handleSeatboxLockChange(true)
	return r, a
}

func waitDriveLatchState(t *testing.T, r *BatteryReader, state fsm.State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.fsm.State() == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("state = %s, want %s", r.fsm.State(), state)
}

func TestSeatboxOpeningKeepsDriveHeartbeats(t *testing.T) {
	for _, index := range []int{0, 1} {
		t.Run(fmt.Sprintf("slot%d", index), func(t *testing.T) {
			r, actions := newDriveLatchReader(index)
			r.handleVehicleStateChange(VehicleStateReadyToDrive)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if err := r.fsm.Start(ctx); err != nil {
				t.Fatal(err)
			}
			r.fsm.SendEvent(fsm.EvInitComplete)
			waitDriveLatchState(t, r, fsm.StateWaitArrival)
			r.fsm.SendEvent(fsm.EvTagArrived)
			waitDriveLatchState(t, r, fsm.StateWaitUpdate)

			for _, closed := range []bool{false, true, false, false} {
				r.handleSeatboxLockChange(closed)
				if r.seatboxLockClosed != closed || !r.GetSeatboxLockClosed() || !r.GetEnabled() || !r.ShouldSendOn() {
					t.Fatalf("raw closed=%t: latch=%t enabled=%t sendOn=%t", closed, r.GetSeatboxLockClosed(), r.GetEnabled(), r.ShouldSendOn())
				}
				select {
				case <-r.restartChan:
					t.Fatal("seatbox report restarted the drive heartbeat")
				default:
				}
			}

			// A fresh heartbeat and a full presence-check restart must both use
			// the held latch, rather than re-reading the open sensor as an OFF.
			r.fsm.SendEvent(fsm.EvHeartbeatTimeout)
			waitDriveLatchState(t, r, fsm.StateSendClosed)
			waitDriveLatchState(t, r, fsm.StateWaitUpdate)
			r.handleRestart()
			waitDriveLatchState(t, r, fsm.StateSendClosed)
			waitDriveLatchState(t, r, fsm.StateWaitUpdate)
			commands := actions.snapshot()
			if len(commands) != 6 {
				t.Fatalf("commands = %v, want three closed/ON pairs", commands)
			}
			for i, cmd := range commands {
				want := fsm.BMSCmdSeatboxClosed
				if i%2 == 1 {
					want = fsm.BMSCmdOn
				}
				if cmd != want {
					t.Fatalf("command %d = %s, want %s", i, cmd, want)
				}
			}

			r.handleVehicleStateChange(VehicleStateParked)
			if r.GetSeatboxLockClosed() {
				t.Fatal("drive latch remained closed after parking with the seatbox open")
			}
			select {
			case <-r.restartChan:
				r.handleRestart()
			default:
				t.Fatal("parking did not resume normal seatbox handling")
			}
			waitDriveLatchState(t, r, fsm.StateSendOpened)
			commands = actions.snapshot()
			if len(commands) != 8 || commands[6] != fsm.BMSCmdOff || commands[7] != fsm.BMSCmdSeatboxOpened {
				t.Fatalf("commands after parking = %v, want OFF then seatbox-opened", commands)
			}
		})
	}
}

func TestDriveSeatboxLatchPreservesOffDecisions(t *testing.T) {
	for _, reason := range []string{"empty", "voltage-delta", "disabled"} {
		t.Run(reason, func(t *testing.T) {
			r, _ := newDriveLatchReader(0)
			r.handleVehicleStateChange(VehicleStateReadyToDrive)
			if reason == "voltage-delta" {
				a := newAdmissionTestPolicy()
				now := time.Now()
				observeWritten(a, 0, admissionFrame(51000, BMSStateActive), now)
				observeWritten(a, 1, admissionFrame(55000, BMSStateActive), now)
				if !a.shouldIsolate(0, now, DefaultMaxVoltageDeltaMV) || !a.isolationOffAttempt(0, now) {
					t.Fatal("failed to start parked isolation")
				}
				a.setVehicle(VehicleStateReadyToDrive)
				r.service.admission = a
				r.service.config.MaxVoltageDelta.Store(DefaultMaxVoltageDeltaMV)
			}
			r.handleSeatboxLockChange(false)
			switch reason {
			case "empty":
				r.data.LowSOC = true
				r.data.Charge = 0
			case "disabled":
				r.handleEnabledChange(false)
			}
			if !r.GetSeatboxLockClosed() || r.ShouldSendOn() {
				t.Fatalf("latch=%t sendOn=%t: drive latch must not override %s", r.GetSeatboxLockClosed(), r.ShouldSendOn(), reason)
			}
		})
	}
}
