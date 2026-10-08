package battery

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"battery-service/battery/fsm"
)

func admissionTestReader(a *packAdmission, index int) *BatteryReader {
	config := &ServiceConfig{}
	config.MaxVoltageDelta.Store(DefaultMaxVoltageDeltaMV)
	return &BatteryReader{
		index: index, enabled: true,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		service: &Service{config: config, admission: a},
	}
}

func TestAdmissionCommandBoundary(t *testing.T) {
	a := newAdmissionTestPolicy()
	r := admissionTestReader(a, 0)
	for _, cmd := range []fsm.BMSCommand{fsm.BMSCmdSeatboxClosed, fsm.BMSCmdInsertedInScooter, fsm.BMSCmdSeatboxOpened, fsm.BMSCmdOff} {
		if !r.admitCommand(cmd, admissionTestTime) {
			t.Fatalf("non-ON command %v was blocked", cmd)
		}
	}
	if r.admitCommand(fsm.BMSCmdOn, admissionTestTime) {
		t.Fatal("ON bypassed missing observations")
	}
	observeWritten(a, 0, admissionFrame(53000, BMSStateIdle), admissionTestTime)
	observeWritten(a, 1, admissionFrame(55000, BMSStateIdle), admissionTestTime)
	if r.admitCommand(fsm.BMSCmdOn, admissionTestTime) {
		t.Fatal("lower-voltage pack bypassed selection")
	}
	if !admissionTestReader(a, 1).admitCommand(fsm.BMSCmdOn, admissionTestTime) {
		t.Fatal("selected pack was blocked")
	}
	if r.admitCommand(fsm.BMSCmdOn, admissionTestTime) {
		t.Fatal("ON bypassed peer reservation")
	}
}

func TestAdmissionWaitingDoesNotRequestOffOrRecovery(t *testing.T) {
	a := newAdmissionTestPolicy()
	r := admissionTestReader(a, 0)
	now := time.Now()
	r.data = admissionFrame(51000, BMSStateIdle)
	observeWritten(a, 0, r.data, now)
	observeWritten(a, 1, admissionFrame(55000, BMSStateActive), now)
	if !r.ShouldSendOn() {
		t.Fatal("admission waiting must not select the FSM's OFF branch")
	}
	if r.admitCommand(fsm.BMSCmdOn, now) {
		t.Fatal("incompatible pack was admitted")
	}
	if !r.CheckStateCorrect() {
		t.Fatal("a waiting idle pack must not enter the mismatch recovery loop")
	}
	// Seatbox/explicit disable still expects the pack to be OFF.
	r.enabled = false
	r.data.State = BMSStateActive
	if r.ShouldSendOn() || r.CheckStateCorrect() {
		t.Fatal("admission waiting bypassed explicit OFF behavior")
	}
}

func TestAdmissionOffCommandInvalidatesOnPermission(t *testing.T) {
	a := newAdmissionTestPolicy()
	r := admissionTestReader(a, 0)
	observeWritten(a, 0, admissionFrame(53000, BMSStateActive), admissionTestTime)
	if !r.admitCommand(fsm.BMSCmdOn, admissionTestTime) {
		t.Fatal("running pack lost its heartbeat")
	}
	now := admissionTestTime.Add(time.Second)
	if !r.admitCommand(fsm.BMSCmdOff, now) {
		t.Fatal("OFF was blocked")
	}
	if r.admitCommand(fsm.BMSCmdOn, now) {
		t.Fatal("OFF followed by ON reused admission without confirmation")
	}
}

func TestAdmissionReservationExpiresWithoutOnConfirmation(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(55000, BMSStateIdle), admissionTestTime)
	observeWritten(a, 1, admissionFrame(53000, BMSStateIdle), admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, true)
	now := admissionTestTime.Add(admissionOnSettle + time.Second)
	requireAdmission(t, a, 0, now, false)
	// An ambiguous command remains a possible live supply, not proof of OFF.
	observeWritten(a, 1, admissionFrame(53000, BMSStateIdle), now)
	requireAdmission(t, a, 1, now, false)
}

func TestAdmissionOffNeedsPostCommandEvidence(t *testing.T) {
	a := newAdmissionTestPolicy()
	frame := admissionFrame(53000, BMSStateIdle)
	observeWritten(a, 0, frame, admissionTestTime)
	a.observeAbsent(1, admissionTestTime)
	requireAdmission(t, a, 0, admissionTestTime, true)
	now := admissionTestTime.Add(time.Second)
	a.offAttempt(0, now)
	// Re-reading the pre-command idle snapshot cannot confirm OFF.
	a.observe(0, frame, now.Add(time.Second))
	requireAdmission(t, a, 0, now.Add(time.Second), false)
	frame.Voltage++
	a.observe(0, frame, now.Add(2*time.Second))
	requireAdmission(t, a, 0, now.Add(2*time.Second), true)
}
