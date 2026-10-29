package battery

import (
	"context"
	"fmt"
	"testing"
	"time"

	"battery-service/battery/fsm"
)

func TestAdmissionUnconfirmedTagNeedsWakeWithoutOn(t *testing.T) {
	for _, state := range []BMSState{BMSStateIdle, BMSStateAsleep} {
		for _, identity := range []string{"first observation", "same tag", "different tag"} {
			t.Run(state.String()+"/"+identity, func(t *testing.T) {
				a := newAdmissionTestPolicy()
				r := admissionTestReader(a, 0)
				now := time.Now()
				r.data = admissionFrame(53000, state)
				a.tag(0, []byte("first"))
				if identity != "first observation" {
					observeWritten(a, 0, r.data, now)
					uid := "first"
					if identity == "different tag" {
						uid = "second"
					}
					a.tag(0, []byte(uid))
				}
				a.observe(0, r.data, now)
				a.observeAbsent(1, now)
				if r.admitCommand(fsm.BMSCmdOn, now) {
					t.Fatal("unconfirmed tag allowed ON")
				}
				if r.CheckStateCorrect() {
					t.Fatal("unconfirmed inactive tag skipped the wake-up path")
				}
				if !r.admitCommand(fsm.BMSCmdInsertedInScooter, now) {
					t.Fatal("wake-up command was blocked")
				}
				if a.slots[0].held || a.slots[0].offPending {
					t.Fatal("wake-up changed supply tracking")
				}
			})
		}
	}
}

// Model a tag that retains its cached frame until INSERTED wakes the BMS.
// Keep the real FSM, reader state check and ON admission boundary.
type admissionWakeActions struct {
	*driveLatchActions
	wakePending bool
	onPending   bool
	blockedOn   int
}

func (a *admissionWakeActions) WriteCommand(cmd fsm.BMSCommand) {
	if !a.admitCommand(cmd, time.Now()) {
		if cmd == fsm.BMSCmdOn {
			a.blockedOn++
		}
		return
	}
	a.driveLatchActions.WriteCommand(cmd)
	if cmd == fsm.BMSCmdInsertedInScooter {
		a.wakePending = true
	}
	if cmd == fsm.BMSCmdOn {
		a.onPending = true
	}
}

func (a *admissionWakeActions) ReadStatus() error {
	if a.wakePending {
		a.data.State = BMSStateIdle
		a.data.Voltage++
		a.wakePending = false
	}
	if a.onPending {
		a.data.State = BMSStateActive
		a.onPending = false
	}
	now := time.Now()
	policy := a.packAdmission()
	id := policy.claimRound(a.index, now)
	if policy.observeAcquisition(a.index, id, acquisition{start: now, end: now, complete: now, data: a.data}) {
		a.service.notifyAdmissionReady()
	}
	return nil
}

func TestAdmissionFSMWakesUnconfirmedTagBeforeOn(t *testing.T) {
	for _, slot := range []int{0, 1} {
		for _, state := range []BMSState{BMSStateIdle, BMSStateAsleep} {
			t.Run(fmt.Sprintf("slot%d/%s", slot, state), func(t *testing.T) {
				r, base := newDriveLatchReader(slot)
				a := newAdmissionTestPolicy()
				r.service.admission = a
				r.service.logger = r.logger
				r.service.config.MaxVoltageDelta.Store(DefaultMaxVoltageDeltaMV)
				r.data = admissionFrame(53000, state)
				a.observe(slot, r.data, time.Now())
				a.observeAbsent(1-slot, time.Now())
				actions := &admissionWakeActions{driveLatchActions: base}
				r.fsm = fsm.New(actions, r.logger)
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				if err := r.fsm.Start(ctx); err != nil {
					t.Fatal(err)
				}
				r.fsm.SendEvent(fsm.EvInitComplete)
				waitDriveLatchState(t, r, fsm.StateWaitArrival)
				r.fsm.SendEvent(fsm.EvTagArrived)
				waitDriveLatchState(t, r, fsm.StateWaitUpdate)
				commands := actions.snapshot()
				inserted := 0
				for _, cmd := range commands {
					if cmd == fsm.BMSCmdOn || cmd == fsm.BMSCmdOff {
						t.Fatalf("power command before refreshed tag: %v", commands)
					}
					if cmd == fsm.BMSCmdInsertedInScooter {
						inserted++
					}
				}
				if inserted != 1 || actions.blockedOn == 0 {
					t.Fatalf("wake-ups=%d blocked ONs=%d, commands=%v", inserted, actions.blockedOn, commands)
				}
				if !a.slots[slot].valid || a.slots[slot].held {
					t.Fatal("wake-up must refresh the tag without reserving power")
				}
				r.fsm.SendEvent(fsm.EvAdmissionPoll)
				waitDriveLatchState(t, r, fsm.StateSendClosed)
				waitDriveLatchState(t, r, fsm.StateWaitUpdate)
				commands = actions.snapshot()
				if commands[len(commands)-1] != fsm.BMSCmdOn || r.data.State != BMSStateActive {
					t.Fatalf("refreshed tag did not activate: %v", commands)
				}
			})
		}
	}
}
