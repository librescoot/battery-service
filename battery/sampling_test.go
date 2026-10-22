package battery

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func samplingPolicy() *packAdmission {
	a := newAdmissionTestPolicy()
	a.sampling.enabled = true
	for i := range a.slots {
		state := BMSStateIdle
		if i == 0 {
			state = BMSStateActive
		}
		observeWritten(a, i, admissionFrame(52000, state), admissionTestTime.Add(-time.Second))
	}
	return a
}

func roundSample(slot int, start, end, complete time.Time, voltage uint) acquisition {
	state := BMSStateIdle
	if slot == 0 {
		state = BMSStateActive
	}
	return acquisition{start: start, end: end, complete: complete, data: admissionFrame(voltage, state)}
}

func deliverPair(t *testing.T, a *packAdmission, now time.Time, voltage uint) {
	t.Helper()
	id, _ := a.beginRound(now)
	for i := 0; i < 2; i++ {
		if got := a.claimRound(i, now); got != id || id == 0 {
			t.Fatalf("claim=%d, round=%d", got, id)
		}
		a.observeAcquisition(i, id, roundSample(i, now, now.Add(100*time.Millisecond), now.Add(200*time.Millisecond), voltage))
	}
	if a.sampling.report.reason != "paired" {
		t.Fatalf("round failed: %+v", a.sampling.report)
	}
	if a.sampling.pairedID != id || a.slots[0].observedRound != id || a.slots[1].observedRound != id {
		t.Fatal("paired evidence lost its round identity")
	}
}

func TestSamplingAcquisitionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		offset, duration, completion time.Duration
		want                         bool
	}{
		{"exact skew", 900 * time.Millisecond, 100 * time.Millisecond, time.Second, true},
		{"excess skew", 900*time.Millisecond + time.Nanosecond, 100 * time.Millisecond, time.Second + time.Nanosecond, false},
		{"slow read same start", 0, time.Second + time.Nanosecond, time.Second + time.Nanosecond, false},
		{"full frame boundary", 0, 100 * time.Millisecond, samplingFrameLimit, true},
		{"late frame", 0, 100 * time.Millisecond, samplingFrameLimit + time.Nanosecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := samplingPolicy()
			now := admissionTestTime
			id, _ := a.beginRound(now)
			a.claimRound(0, now)
			a.claimRound(1, now)
			a.observeAcquisition(0, id, roundSample(0, now, now.Add(100*time.Millisecond), now.Add(200*time.Millisecond), 52001))
			start := now.Add(tc.offset)
			a.observeAcquisition(1, id, roundSample(1, start, start.Add(tc.duration), now.Add(tc.completion), 52001))
			if got := !a.sampling.pairedAt.IsZero(); got != tc.want {
				t.Fatalf("paired=%t report=%+v", got, a.sampling.report)
			}
			requireAdmission(t, a, 0, now.Add(tc.completion), true)
			requireAdmission(t, a, 1, now.Add(tc.completion), false)
		})
	}
}

func TestSamplingMatchingCompletionDoesNotHideSkew(t *testing.T) {
	a := samplingPolicy()
	now := admissionTestTime
	id, _ := a.beginRound(now)
	complete := now.Add(1500 * time.Millisecond)
	for i := 0; i < 2; i++ {
		start := now.Add(time.Duration(i) * 1100 * time.Millisecond)
		a.claimRound(i, start)
		a.observeAcquisition(i, id, roundSample(i, start, start.Add(100*time.Millisecond), complete, 52001))
	}
	if !a.sampling.pairedAt.IsZero() || a.sampling.report.skew != 1200*time.Millisecond {
		t.Fatalf("completion timestamps hid skew: %+v", a.sampling.report)
	}
}

func TestSamplingStableAdmissionAndPendingFrame(t *testing.T) {
	a := samplingPolicy()
	now := admissionTestTime
	for i := 0; i < 3; i++ {
		deliverPair(t, a, now.Add(time.Duration(i)*admissionPollInterval), 52001+uint(i%2))
	}
	at := now.Add(6*time.Second + 200*time.Millisecond)
	// A new unpaired frame must not borrow the previous round's peer.
	id, _ := a.beginRound(now.Add(9 * time.Second))
	a.claimRound(0, now.Add(9*time.Second))
	a.observeAcquisition(0, id, roundSample(0, now.Add(9*time.Second), now.Add(9100*time.Millisecond), now.Add(9200*time.Millisecond), 52002))
	requireAdmission(t, a, 1, now.Add(9200*time.Millisecond), false)
	a.claimRound(1, now.Add(9*time.Second))
	a.observeAcquisition(1, id, roundSample(1, now.Add(9*time.Second), now.Add(9100*time.Millisecond), now.Add(9200*time.Millisecond), 52002))
	requireAdmission(t, a, 1, at.Add(3*time.Second), true)
	if a.sampling.round != nil || !a.sampling.pairedAt.IsZero() || a.sampling.pairedID != 0 {
		t.Fatal("reservation retained pairing evidence")
	}
}

func TestSamplingFailureContinuityAndCancellation(t *testing.T) {
	for _, action := range []string{"missing", "absent", "failed", "OFF", "vehicle", "role", "tag", "shutdown", "late", "stale"} {
		t.Run(action, func(t *testing.T) {
			a := samplingPolicy()
			now := admissionTestTime
			id, _ := a.beginRound(now)
			a.claimRound(0, now)
			a.claimRound(1, now)
			sample := roundSample(0, now, now.Add(100*time.Millisecond), now.Add(200*time.Millisecond), 52001)
			a.observeAcquisition(0, id, sample)
			switch action {
			case "missing":
				a.beginRound(now.Add(samplingDeadline + admissionPollInterval))
			case "absent":
				a.observeAbsent(1, now)
			case "failed":
				a.invalidate(1)
			case "OFF":
				a.offAttempt(1, now)
			case "vehicle":
				a.setVehicle(VehicleStateReadyToDrive)
			case "role":
				a.setRole(1, false)
			case "tag":
				a.tag(1, []byte{2})
			case "shutdown":
				a.stopSampling()
			case "late":
				a.beginRound(now.Add(samplingDeadline + admissionPollInterval))
			case "stale":
				a.slots[1].written = now.Add(-admissionMaxAge - time.Second)
			}
			peer := roundSample(1, now, now.Add(100*time.Millisecond), now.Add(200*time.Millisecond), 52000)
			a.observeAcquisition(1, id, peer)
			if !a.sampling.pairedAt.IsZero() {
				t.Fatal("invalid round paired")
			}
			requireAdmission(t, a, 1, now.Add(time.Second), false)
			requireAdmission(t, a, 0, now.Add(time.Second), true)
			if !a.slots[0].held || !a.slots[0].active || a.slots[0].offPending {
				t.Fatal("round failure altered running supply")
			}
			if action == "shutdown" {
				if next, _ := a.beginRound(now.Add(3 * time.Second)); next != 0 {
					t.Fatal("round after shutdown")
				}
			}
		})
	}
}

func TestSamplingRejectsUnsafeAndStaleWindows(t *testing.T) {
	for _, kind := range []string{"load", "voltage", "moving", "stale"} {
		t.Run(kind, func(t *testing.T) {
			a := samplingPolicy()
			now := admissionTestTime
			for i := 0; i < 3; i++ {
				at := now.Add(time.Duration(i) * admissionPollInterval)
				id, _ := a.beginRound(at)
				for slot := 0; slot < 2; slot++ {
					a.claimRound(slot, at)
					sample := roundSample(slot, at, at.Add(100*time.Millisecond), at.Add(200*time.Millisecond), 52001+uint(i%2))
					if kind == "load" && i == 1 {
						sample.data.Current = admissionMaxCurrent + 1
					}
					if kind == "voltage" && slot == 1 {
						sample.data.Voltage += 2000
					}
					a.observeAcquisition(slot, id, sample)
				}
			}
			at := now.Add(6200 * time.Millisecond)
			if kind == "moving" {
				a.setVehicle(VehicleStateReadyToDrive)
			}
			if kind == "stale" {
				at = at.Add(admissionMaxAge + time.Second)
			}
			requireAdmission(t, a, 1, at, false)
			requireAdmission(t, a, 0, at, kind != "voltage")
			if kind == "voltage" && !a.shouldIsolate(0, at, DefaultMaxVoltageDeltaMV) {
				t.Fatal("higher-voltage handover did not request the lower pack OFF")
			}
		})
	}
}

func TestSamplingOngoingAndSinglePack(t *testing.T) {
	a := samplingPolicy()
	a.slots[1].active = true
	a.slots[1].held = true
	for _, state := range []VehicleState{
		VehicleStateStandby, VehicleStateParked, VehicleStateReadyToDrive,
		VehicleStateWaitingSeatbox, VehicleStateShuttingDown, VehicleStateUpdating,
		VehicleStateWaitingHibernation, VehicleStateWaitingHibernationAdvanced,
		VehicleStateWaitingHibernationSeatbox, VehicleStateWaitingHibernationConfirm,
		VehicleStateOther, "",
	} {
		a.setVehicle(state)
		if !a.needsPoll() {
			t.Fatalf("active packs no longer sampled in %q", state)
		}
		if id, _ := a.beginRound(admissionTestTime); id == 0 {
			t.Fatalf("no sampling round in %q", state)
		}
	}
	a.setRole(1, false)
	if id, _ := a.beginRound(admissionTestTime); id == 0 {
		t.Fatal("inactive-role peer still needs sampling for OFF proof")
	}
	a = samplingPolicy()
	a.sampling.enabled = false
	if id, _ := a.beginRound(admissionTestTime); id != 0 {
		t.Fatal("disabled round")
	}
}

func TestSamplingStandbyPairsDoNotAdmitSecondPack(t *testing.T) {
	a := samplingPolicy()
	a.setVehicle(VehicleStateStandby)
	for i := 0; i < 3; i++ {
		deliverPair(t, a, admissionTestTime.Add(time.Duration(i)*admissionPollInterval), 52001+uint(i%2))
	}
	now := admissionTestTime.Add(6200 * time.Millisecond)
	requireAdmission(t, a, 0, now, true)
	if allowed, reason := a.requestOn(1, now, DefaultMaxVoltageDeltaMV); allowed || reason != "waiting for parked" {
		t.Fatalf("standby admission: allowed=%t reason=%q", allowed, reason)
	}
	a.setVehicle(VehicleStateParked)
	requireAdmission(t, a, 1, now, false)
	for i := 3; i < 6; i++ {
		deliverPair(t, a, admissionTestTime.Add(time.Duration(i)*admissionPollInterval), 52001+uint(i%2))
	}
	requireAdmission(t, a, 1, admissionTestTime.Add(15200*time.Millisecond), true)
}

func TestSamplingConcurrentClaimsAndResults(t *testing.T) {
	a := samplingPolicy()
	now := admissionTestTime
	id, _ := a.beginRound(now)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			if got := a.claimRound(slot, now); got != id {
				t.Errorf("claim=%d", got)
			}
			if got := a.claimRound(slot, now); got != 0 {
				t.Errorf("duplicate claim=%d", got)
			}
			a.observeAcquisition(slot, id, roundSample(slot, now, now.Add(100*time.Millisecond), now.Add(200*time.Millisecond), 52001))
		}(i)
	}
	wg.Wait()
	if a.sampling.report.reason != "paired" {
		t.Fatalf("report=%+v", a.sampling.report)
	}
}

func TestSamplingPreparationDelayAndNoOverlappingRounds(t *testing.T) {
	for _, late := range []bool{false, true} {
		a := samplingPolicy()
		now := admissionTestTime
		id, _ := a.beginRound(now)
		if next, _ := a.beginRound(now.Add(admissionPollInterval)); next != 0 || a.sampling.round.id != id {
			t.Fatal("unfinished round replaced or re-prompted")
		}
		start := now.Add(samplingDeadline - 200*time.Millisecond)
		complete := now.Add(samplingDeadline)
		if late {
			complete = complete.Add(time.Nanosecond)
		}
		for i := 0; i < 2; i++ {
			a.claimRound(i, start)
			a.observeAcquisition(i, id, roundSample(i, start, start.Add(100*time.Millisecond), complete, 52001))
		}
		if got := !a.sampling.pairedAt.IsZero(); got == late {
			t.Fatalf("late=%t paired=%t report=%+v", late, got, a.sampling.report)
		}
	}
}

func TestSamplingLongerCadenceRetainsStableWindow(t *testing.T) {
	a := samplingPolicy()
	for i := 0; i < 3; i++ {
		deliverPair(t, a, admissionTestTime.Add(time.Duration(i)*9*time.Second), 52001+uint(i%2))
	}
	requireAdmission(t, a, 1, admissionTestTime.Add(18200*time.Millisecond), true)
}

func TestSamplingStabilityUsesAcquisitionSpan(t *testing.T) {
	a := samplingPolicy()
	for i, offset := range []time.Duration{0, 3 * time.Second, 5900 * time.Millisecond} {
		deliverPair(t, a, admissionTestTime.Add(offset), 52001+uint(i%2))
	}
	// Completion and time spent waiting cannot stretch 5.9 seconds of
	// observations into the required six-second stable window.
	requireAdmission(t, a, 1, admissionTestTime.Add(7*time.Second), false)
}

func TestSamplingMissingPackAndOffConfirmation(t *testing.T) {
	a := newAdmissionTestPolicy()
	a.sampling.enabled = true
	now := admissionTestTime
	observeWritten(a, 0, admissionFrame(52000, BMSStateIdle), now)
	a.observeAbsent(1, now)
	requireAdmission(t, a, 0, now, true)
	if !a.slots[0].held {
		t.Fatal("first supply not reserved")
	}
	a.offAttempt(0, now)
	a.invalidate(0)
	if !a.slots[0].held || !a.slots[0].offPending {
		t.Fatal("read failure released OFF reservation")
	}
	// OFF confirmation must work even with no sampling round.
	a.observeAcquisition(0, 0, acquisition{data: admissionFrame(52001, BMSStateIdle), complete: now.Add(time.Second)})
	if a.slots[0].held || a.slots[0].offPending {
		t.Fatal("unpaired fresh OFF not confirmed")
	}
}

func TestSamplingDiagnosticsThroughServiceHandler(t *testing.T) {
	var out bytes.Buffer
	s := &Service{logger: slog.New(NewServiceHandler(&out, LogLevelDebug))}
	s.logSamplingReport(samplingReport{})
	if out.Len() != 0 {
		t.Fatal("empty report logged")
	}
	s.logSamplingReport(samplingReport{
		id: 7, reason: "acquisition skew", skew: 1200 * time.Millisecond,
		status0: [2]time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
		frame:   [2]time.Duration{300 * time.Millisecond, 400 * time.Millisecond},
	})
	for _, field := range []string{"round=7", `result="acquisition skew"`, "skew=1.2s", "status0_duration=[100ms 200ms]", "frame_duration=[300ms 400ms]"} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("missing %q in %q", field, out.String())
		}
	}
}

func TestSamplingExpiredClaimAndFailureDiagnostics(t *testing.T) {
	a := samplingPolicy()
	now := admissionTestTime
	id, _ := a.beginRound(now)
	if got := a.claimRound(0, now.Add(samplingDeadline+time.Nanosecond)); got != 0 {
		t.Fatal("expired claim accepted")
	}
	sample := acquisition{start: now, end: now.Add(300 * time.Millisecond), complete: now.Add(400 * time.Millisecond)}
	a.failAcquisition(0, id, sample)
	_, report := a.beginRound(now.Add(admissionPollInterval))
	if report.id != id || report.reason != "read failed" || report.status0[0] != 300*time.Millisecond || report.frame[0] != 400*time.Millisecond {
		t.Fatalf("failure diagnostics=%+v", report)
	}
}
