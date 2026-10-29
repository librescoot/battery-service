package battery

import (
	"context"
	"testing"
	"time"

	"battery-service/battery/fsm"
)

func TestDriveStartDemandReadsPublishedStatus(t *testing.T) {
	values := map[string]string{"vehicle/drive-start-status": "waiting-battery"}
	s, _ := admissionSettingService(t, values)
	s.admissionRequests = make(chan struct{}, 1)
	s.handleDriveStartStatusMessage()
	if !s.driveStartDemandActive() || len(s.admissionRequests) != 1 {
		t.Fatal("published battery wait did not request status")
	}
	values["vehicle/drive-start-status"] = "waiting-ecu"
	s.handleDriveStartStatusMessage()
	if s.driveStartDemandActive() {
		t.Fatal("non-battery wait did not withdraw the request")
	}
	delete(values, "vehicle/drive-start-status")
	s.handleDriveStartStatusMessage()
	if s.driveStartDemandActive() {
		t.Fatal("missing status created a request")
	}
}

func TestDriveStartDemandCoalescesAndExpires(t *testing.T) {
	s := &Service{admissionRequests: make(chan struct{}, 1), admissionReady: make(chan struct{}, 1)}
	for _, status := range []string{"", "waiting-ecu", "waiting-power", "close-seatbox", "timeout"} {
		s.setDriveStartStatus(status)
		if s.driveStartDemandActive() || len(s.admissionRequests) != 0 {
			t.Fatalf("unexpected demand for %q", status)
		}
	}
	s.setDriveStartStatus("waiting-battery")
	deadline := s.driveStartDeadline.Load()
	for i := 0; i < 100; i++ {
		s.setDriveStartStatus("waiting-battery")
		s.notifyAdmissionReady()
	}
	if !s.driveStartDemandActive() || len(s.admissionRequests) != 1 || len(s.admissionReady) != 1 || s.driveStartDeadline.Load() != deadline {
		t.Fatal("demand was not coalesced with a fixed deadline")
	}
	<-s.admissionRequests
	<-s.admissionReady
	expired := time.Now().Add(-time.Second)
	s.driveStartDeadline.Store(&expired)
	s.setDriveStartStatus("waiting-battery")
	s.notifyAdmissionReady()
	if s.driveStartDemandActive() || len(s.admissionRequests) != 0 || len(s.admissionReady) != 0 {
		t.Fatal("expired demand was extended or retried")
	}
	s.setDriveStartStatus("")
	s.setDriveStartStatus("waiting-battery")
	if !s.driveStartDemandActive() || len(s.admissionRequests) != 1 {
		t.Fatal("new rider request did not start a new demand")
	}
	s.setDriveStartStatus("timeout")
	if s.driveStartDemandActive() {
		t.Fatal("withdrawn demand remained active")
	}
}

func startDemandLoop(t *testing.T, s *Service, ctx context.Context, cancel context.CancelFunc) {
	t.Helper()
	s.ctx = ctx
	s.admissionRequests = make(chan struct{}, 1)
	s.admissionReady = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runAdmissionRequests(nil) // No periodic ticks can satisfy these tests.
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func waitDemandCommands(t *testing.T, a *driveLatchActions, count int, timeout time.Duration) []fsm.BMSCommand {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if commands := a.snapshot(); len(commands) >= count {
			return commands
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no demand-driven command: got %v, want at least %d commands", a.snapshot(), count)
	return nil
}

func TestDriveStartDemandPromptsStatusWithoutPeriodicTick(t *testing.T) {
	r, actions := newDriveLatchReader(0)
	s := r.service
	s.logger = r.logger
	s.admission = newAdmissionTestPolicy()
	s.admission.sampling.enabled = true
	s.readers = []*BatteryReader{r}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := r.fsm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.fsm.SendEvent(fsm.EvInitComplete)
	waitDriveLatchState(t, r, fsm.StateWaitArrival)
	r.fsm.SendEvent(fsm.EvTagArrived)
	waitDriveLatchState(t, r, fsm.StateWaitUpdate)
	before := len(actions.snapshot())
	startDemandLoop(t, s, ctx, cancel)
	s.setDriveStartStatus("waiting-battery")
	commands := waitDemandCommands(t, actions, before+1, 250*time.Millisecond)
	if commands[before] != fsm.BMSCmdSeatboxClosed {
		t.Fatalf("demand bypassed normal heartbeat sequencing: %v", commands)
	}
}

func TestDriveStartDemandRetriesPairedEvidenceWithoutPeriodicTick(t *testing.T) {
	a := newAdmissionTestPolicy()
	a.conservative = false
	a.sampling.enabled = true
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var s *Service
	var actions [2]*admissionWakeActions
	for slot := range actions {
		r, base := newDriveLatchReader(slot)
		if s == nil {
			s = r.service
			s.logger = r.logger
			s.admission = a
			s.config.MaxVoltageDelta.Store(DefaultMaxVoltageDeltaMV)
		}
		r.service = s
		state := BMSStateIdle
		if slot == 1 {
			state = BMSStateActive
		}
		r.data = admissionFrame(53000, state)
		observeWritten(a, slot, r.data, time.Now())
		actions[slot] = &admissionWakeActions{driveLatchActions: base}
		r.fsm = fsm.New(actions[slot], r.logger)
		s.readers = append(s.readers, r)
	}
	for _, r := range s.readers {
		if err := r.fsm.Start(ctx); err != nil {
			t.Fatal(err)
		}
		r.fsm.SendEvent(fsm.EvInitComplete)
		waitDriveLatchState(t, r, fsm.StateWaitArrival)
		r.fsm.SendEvent(fsm.EvTagArrived)
	}
	for _, r := range s.readers {
		waitDriveLatchState(t, r, fsm.StateWaitUpdate)
	}
	for _, cmd := range actions[0].snapshot() {
		if cmd == fsm.BMSCmdOn {
			t.Fatal("second pack admitted without paired acquisition")
		}
	}
	startDemandLoop(t, s, ctx, cancel)
	s.setDriveStartStatus("waiting-battery")
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, cmd := range actions[0].snapshot() {
			if cmd == fsm.BMSCmdOn {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("paired evidence did not trigger ON without a periodic tick: %v", actions[0].snapshot())
}

func TestAdmissionProgressRequiresNewEvidence(t *testing.T) {
	a := newAdmissionTestPolicy()
	now := time.Now()
	frame := admissionFrame(53000, BMSStateIdle)
	sample := acquisition{start: now, end: now, complete: now, data: frame}
	if a.observeAcquisition(0, 0, sample) {
		t.Fatal("unconfirmed first frame prompted admission retry")
	}
	sample.data.Voltage++
	if !a.observeAcquisition(0, 0, sample) {
		t.Fatal("first confirmed frame did not prompt admission retry")
	}
	if a.observeAcquisition(0, 0, sample) {
		t.Fatal("unchanged reread created a retry loop")
	}
	sample.data.Voltage++
	if a.observeAcquisition(0, 0, sample) {
		t.Fatal("ordinary value change created a retry loop")
	}
	a.tag(0, []byte("replacement"))
	a.stopSampling()
	a.observeAcquisition(0, 0, sample)
	sample.data.Voltage++
	if a.observeAcquisition(0, 0, sample) {
		t.Fatal("shutdown prompted another admission retry")
	}
}
