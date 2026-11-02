package battery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"battery-service/battery/fsm"
	"github.com/librescoot/pn7150"
	"github.com/redis/go-redis/v9"
)

type absenceRedisHook struct {
	fields map[string]any
	writes int
	fail   bool
}

func (*absenceRedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unexpected Redis connection")
	}
}
func (*absenceRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(context.Context, redis.Cmder) error { return nil }
}
func (h *absenceRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, cmds []redis.Cmder) error {
		if h.fail {
			return errors.New("Redis unavailable")
		}
		for _, cmd := range cmds {
			if cmd.Name() == "hmset" {
				h.writes++
				h.fields = make(map[string]any)
				args := cmd.Args()
				for i := 2; i < len(args); i += 2 {
					h.fields[args[i].(string)] = args[i+1]
				}
			}
		}
		return nil
	}
}

func absenceReader(t *testing.T, index int) (*BatteryReader, *absenceRedisHook) {
	t.Helper()
	r, _ := newDriveLatchReader(index)
	r.ctx = context.Background()
	h := &absenceRedisHook{}
	r.service.redis = redis.NewClient(&redis.Options{Addr: "unused", MaxRetries: -1})
	r.service.redis.AddHook(h)
	t.Cleanup(func() { _ = r.service.redis.Close() })
	r.initializeFaultManagement()
	t.Cleanup(r.cleanupFaultManagement)
	t.Cleanup(func() { r.fresh.stopTimer() })
	r.previousTagPresent = true
	r.currentTagUID = []byte{1, 2, 3}
	r.data = liveFrame
	r.sendStatusUpdate()
	return r, h
}

var quietDiscovery = hal.DiscoveryResult{Kind: hal.DiscoveryQuiet}

type recoveryActions struct {
	*driveLatchActions
	final hal.DiscoveryResult
}

func (a *recoveryActions) StartDiscovery() error { a.discovery = discoveryEvidence{}; return nil }
func (a *recoveryActions) FinishDiscovery()      { a.finishDiscovery(a.final, nil) }

func TestRecoveryCompletedDiscoveryClearsPublishedBattery(t *testing.T) {
	for _, index := range []int{0, 1} {
		t.Run(fmt.Sprintf("slot%d", index), func(t *testing.T) {
			r, h := absenceReader(t, index)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			a := &recoveryActions{driveLatchActions: &driveLatchActions{BatteryReader: r}, final: quietDiscovery}
			r.fsm = fsm.New(a, r.logger)
			if err := r.fsm.Start(ctx); err != nil {
				t.Fatal(err)
			}
			r.fsm.SendEvent(fsm.EvInitComplete)
			waitDriveLatchState(t, r, fsm.StateWaitArrival)
			r.fsm.SendEvent(fsm.EvTagArrived)
			waitDriveLatchState(t, r, fsm.StateWaitUpdate)
			r.handleNFCError(hal.NewTagDepartedError("timeout waiting for notification 0x0105"))
			waitDriveLatchState(t, r, fsm.StateNFCReaderOff)
			if !r.data.Present || h.fields["present"] != "true" {
				t.Fatal("timeout cleared battery")
			}
			waitDriveLatchState(t, r, fsm.StateWaitArrival)
			r.fsm.SendEvent(fsm.EvDepartureTimeout)
			waitDriveLatchState(t, r, fsm.StateTagAbsent)
			for i := 0; i < 20; i++ {
				r.processTagArrivalPoll(quietDiscovery, nil)
			}
			if !r.data.Present || h.fields["present"] != "true" {
				t.Fatal("polls cleared battery before the search completed")
			}
			commands := len(a.snapshot())
			r.fsm.SendEvent(fsm.EvCheckReaderTimeout)
			waitDriveLatchState(t, r, fsm.StateNFCReaderOff)
			if r.data != (BMSData{}) || r.previousTagPresent || len(r.currentTagUID) != 0 {
				t.Fatalf("stale battery: %+v", r.data)
			}
			for field, want := range map[string]any{"present": "false", "serial-number": "", "voltage": "0", "charge": "0", "state": "unknown", "sample:valid": "false"} {
				if got := h.fields[field]; got != want {
					t.Errorf("Redis %s=%v, want %v", field, got, want)
				}
			}
			if len(a.snapshot()) != commands {
				t.Fatal("absence cleanup sent a BMS command")
			}
			writes := h.writes
			r.finishDiscovery(quietDiscovery, nil)
			if h.writes != writes {
				t.Fatal("repeated completion republished departure")
			}
		})
	}
}

func TestDiscoveryRetainsBatteryOnUncertainSearch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result hal.DiscoveryResult
		err    error
	}{
		{"partial or unrelated notification", hal.DiscoveryResult{Kind: hal.DiscoveryPending}, nil},
		{"RF deactivation", hal.DiscoveryResult{Kind: hal.DiscoveryDeactivated}, nil},
		{"same tag", hal.DiscoveryResult{Kind: hal.DiscoveryTags, Tags: []hal.Tag{{ID: []byte{1, 2, 3}}}}, nil},
		{"new tag", hal.DiscoveryResult{Kind: hal.DiscoveryTags, Tags: []hal.Tag{{ID: []byte{4, 5, 6}}}}, nil},
		{"transport error", hal.DiscoveryResult{}, hal.NewI2CTimeoutError("timeout")},
		{"controller error", hal.DiscoveryResult{}, hal.NewNCIInvalidDataError("RF failure")},
		{"transient error", hal.DiscoveryResult{}, errors.New("temporary error")},
	} {
		for _, atBoundary := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/boundary=%t", tc.name, atBoundary), func(t *testing.T) {
				r, h := absenceReader(t, 0)
				r.processTagArrivalPoll(quietDiscovery, nil)
				if atBoundary {
					r.finishDiscovery(tc.result, tc.err)
				} else {
					r.processTagArrivalPoll(tc.result, tc.err)
					r.processTagArrivalPoll(quietDiscovery, nil)
					r.finishDiscovery(quietDiscovery, nil)
				}
				if !r.data.Present || r.data.SerialNumber != liveFrame.SerialNumber || h.fields["present"] != "true" {
					t.Fatal("uncertain search cleared battery")
				}
			})
		}
	}
}

func TestDiscoveryRequiresPollingEvidenceAndRetriesPublication(t *testing.T) {
	r, h := absenceReader(t, 0)
	r.finishDiscovery(quietDiscovery, nil)
	if !r.data.Present {
		t.Fatal("watchdog without polling evidence cleared battery")
	}
	r.processTagArrivalPoll(quietDiscovery, nil)
	h.fail = true
	r.finishDiscovery(quietDiscovery, nil)
	if r.data.Present || !r.previousData.Present || h.fields["present"] != "true" {
		t.Fatal("unexpected failed-publication state")
	}
	h.fail = false
	r.finishDiscovery(quietDiscovery, nil)
	if r.previousData.Present || h.fields["present"] != "false" {
		t.Fatal("failed departure was not republished")
	}
}

func TestDiscoveryAbsencePreservesPossiblePowerSupply(t *testing.T) {
	for _, state := range []VehicleState{VehicleStateStandby, VehicleStateParked, VehicleStateReadyToDrive, VehicleStateOther} {
		t.Run(string(state), func(t *testing.T) {
			r, _ := absenceReader(t, 0)
			a := newAdmissionTestPolicy()
			a.vehicle = state
			a.slots[0].held = true
			a.slots[0].active = true
			a.slots[0].lastOnAt = time.Now()
			r.service.admission = a
			r.processTagArrivalPoll(quietDiscovery, nil)
			r.finishDiscovery(quietDiscovery, nil)
			if !a.slots[0].held || !a.slots[0].active || a.slots[0].absent {
				t.Fatal("RF absence established electrical OFF")
			}
			if state == VehicleStateReadyToDrive {
				if ok, why := a.requestOn(0, time.Now(), DefaultMaxVoltageDeltaMV); !ok {
					t.Fatalf("running supply lost keepalive: %s", why)
				}
				if ok, _ := a.requestOn(1, time.Now(), DefaultMaxVoltageDeltaMV); ok {
					t.Fatal("RF absence admitted a new parallel supply")
				}
			}
		})
	}
}

type discoveryControllerStub struct {
	hal.HAL
	result  hal.DiscoveryResult
	err     error
	started chan struct{}
	release chan struct{}
	calls   int
}

func (s *discoveryControllerStub) StartDiscovery(uint) error { return nil }
func (s *discoveryControllerStub) PollDiscovery() (hal.DiscoveryResult, error) {
	if s.started != nil {
		s.calls++
		if s.calls > 1 {
			return quietDiscovery, nil
		}
		close(s.started)
		<-s.release
	}
	return s.result, s.err
}

func TestNormalDiscoveryDoesNotTreatQuietAsRemoval(t *testing.T) {
	r, h := absenceReader(t, 0)
	r.discovery = discoveryEvidence{quiet: true, uncertain: true}
	r.hal = &discoveryControllerStub{result: quietDiscovery}
	if r.discoverBatteryTag() {
		t.Fatal("quiet discovery found a tag")
	}
	if r.discovery != (discoveryEvidence{}) {
		t.Fatal("new discovery retained old evidence")
	}
	if !r.data.Present || h.fields["present"] != "true" {
		t.Fatal("short receive wait cleared a present battery")
	}
}

func TestDiscoveryPollCancelledDuringReceiveIsDiscarded(t *testing.T) {
	for _, tc := range []struct {
		result hal.DiscoveryResult
		err    error
	}{
		{quietDiscovery, nil},
		{hal.DiscoveryResult{Kind: hal.DiscoveryTags, Tags: []hal.Tag{{ID: []byte{9}}}}, nil},
		{hal.DiscoveryResult{Kind: hal.DiscoveryPending}, nil},
		{hal.DiscoveryResult{}, hal.NewI2CTimeoutError("late error")},
	} {
		r, _ := absenceReader(t, 0)
		stub := &discoveryControllerStub{result: tc.result, err: tc.err, started: make(chan struct{}), release: make(chan struct{})}
		r.hal = stub
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan bool, 1)
		go func() { done <- r.PollForTagArrival(ctx) }()
		<-stub.started
		cancel()
		close(stub.release)
		if <-done {
			t.Fatal("cancelled receive continued polling")
		}
		uncertain := tc.err != nil || tc.result.Kind != hal.DiscoveryQuiet
		if r.discovery.uncertain != uncertain || !r.data.Present || r.currentTagUID[0] != 1 {
			t.Fatal("late receive lost uncertainty or changed battery identity")
		}
		if uncertain {
			r.discovery.quiet = true
			r.finishDiscovery(quietDiscovery, nil)
			if !r.data.Present {
				t.Fatal("cancelled error was mistaken for an empty search")
			}
		}
	}
}

type interruptedDiscoveryActions struct {
	*driveLatchActions
	cancelled chan (<-chan struct{})
}

func (a *interruptedDiscoveryActions) StartDiscovery() error {
	if err := a.BatteryReader.StartDiscovery(); err != nil {
		return err
	}
	a.nfcMu.Lock()
	a.processTagArrivalPoll(quietDiscovery, nil)
	a.nfcMu.Unlock()
	return nil
}
func (a *interruptedDiscoveryActions) FinishDiscovery() { a.BatteryReader.FinishDiscovery() }
func (a *interruptedDiscoveryActions) PollForTagArrival(ctx context.Context) bool {
	a.cancelled <- ctx.Done()
	return a.BatteryReader.PollForTagArrival(ctx)
}

func TestWatchdogDoesNotHideCancelledReceiveError(t *testing.T) {
	r, h := absenceReader(t, 0)
	stub := &discoveryControllerStub{err: hal.NewI2CTimeoutError("receive interrupted at watchdog"), started: make(chan struct{}), release: make(chan struct{})}
	r.hal = stub
	t.Cleanup(func() {
		select {
		case <-stub.release:
		default:
			close(stub.release)
		}
	})
	a := &interruptedDiscoveryActions{driveLatchActions: &driveLatchActions{BatteryReader: r}, cancelled: make(chan (<-chan struct{}), 1)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.fsm = fsm.New(a, r.logger)
	if err := r.fsm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.fsm.SendEvent(fsm.EvInitComplete)
	waitDriveLatchState(t, r, fsm.StateWaitArrival)
	r.fsm.SendEvent(fsm.EvDepartureTimeout)
	select {
	case <-stub.started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	cancelled := <-a.cancelled
	r.fsm.SendEvent(fsm.EvCheckReaderTimeout)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not cancel poll")
	}
	close(stub.release)
	waitDriveLatchState(t, r, fsm.StateNFCReaderOff)
	if !r.data.Present || h.fields["present"] != "true" {
		t.Fatal("watchdog discarded an in-flight error and published removal")
	}
}

func TestFinishDiscoveryChecksForLateActivation(t *testing.T) {
	r, h := absenceReader(t, 0)
	r.processTagArrivalPoll(quietDiscovery, nil)
	r.hal = &discoveryControllerStub{result: hal.DiscoveryResult{Kind: hal.DiscoveryTags, Tags: []hal.Tag{{ID: []byte{1, 2, 3}}}}}
	r.FinishDiscovery()
	if !r.data.Present || h.fields["present"] != "true" {
		t.Fatal("late activation lost to watchdog")
	}
}

func TestRFDeactivationRetainsDataUntilDiscoveryCompletes(t *testing.T) {
	r, h := absenceReader(t, 0)
	r.handleNFCError(hal.NewTagDepartedError("RF deactivated during read"))
	if !r.data.Present || h.fields["present"] != "true" {
		t.Fatal("RF link state was treated as physical pack removal")
	}
}

func TestDiscoveryCleanupPreservesReaderFault(t *testing.T) {
	r, _ := absenceReader(t, 0)
	r.faultStates[BMSFaultBMSCommsError].Present = true
	r.faultStates[BMSFaultNFCReaderError].Present = true
	r.processTagArrivalPoll(quietDiscovery, nil)
	r.finishDiscovery(quietDiscovery, nil)
	if !r.faultStates[BMSFaultBMSCommsError].PendingReset || !r.faultStates[BMSFaultNFCReaderError].Present {
		t.Fatal("departure did not follow existing pack/reader fault policy")
	}
}

func TestCancelledDiscoveryPollDoesNotTouchHardware(t *testing.T) {
	r, _ := absenceReader(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r.PollForTagArrival(ctx) {
		t.Fatal("cancelled poll kept running")
	}
	if !r.data.Present || r.discovery.quiet {
		t.Fatal("cancelled poll changed evidence")
	}
}
