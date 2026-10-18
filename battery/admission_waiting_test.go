package battery

import (
	"testing"
	"time"

	"battery-service/battery/fsm"
)

func TestAdmissionDeferredInactiveStateDoesNotTriggerRecovery(t *testing.T) {
	for _, state := range []BMSState{BMSStateIdle, BMSStateAsleep} {
		for _, evidence := range []string{"first observation", "expired write", "expired observation", "invalidated"} {
			t.Run(state.String()+"/"+evidence, func(t *testing.T) {
				a := newAdmissionTestPolicy()
				r := admissionTestReader(a, 0)
				now := time.Now()
				r.data = admissionFrame(53000, state)
				observeWritten(a, 1, admissionFrame(53020, BMSStateActive), now)
				switch evidence {
				case "first observation":
					a.observe(0, r.data, now)
				case "expired write":
					observeWritten(a, 0, r.data, now.Add(-admissionMaxAge-time.Second))
					a.observe(0, r.data, now)
				case "expired observation":
					observeWritten(a, 0, r.data, now.Add(-admissionMaxAge-time.Second))
				case "invalidated":
					observeWritten(a, 0, r.data, now)
					a.invalidate(0)
				}
				if r.admitCommand(fsm.BMSCmdOn, now) {
					t.Fatal("missing freshness evidence allowed ON")
				}
				if !r.ShouldSendOn() {
					t.Fatal("admission deferral selected OFF")
				}
				if !r.CheckStateCorrect() {
					t.Fatal("deliberately inactive pack entered activation recovery")
				}
				if a.slots[0].held || a.slots[0].offPending {
					t.Fatal("deferral changed supply tracking")
				}
			})
		}
	}
}

func TestAdmissionDeferredStateDoesNotHideActivationFailure(t *testing.T) {
	for _, kind := range []string{"held", "off pending", "unknown", "invalid state", "not coordinated"} {
		t.Run(kind, func(t *testing.T) {
			a := newAdmissionTestPolicy()
			r := admissionTestReader(a, 0)
			r.data = admissionFrame(53000, BMSStateIdle)
			observeWritten(a, 0, r.data, time.Now())
			switch kind {
			case "held":
				a.slots[0].held = true
			case "off pending":
				a.slots[0].held, a.slots[0].offPending = true, true
			case "unknown":
				r.data.State = BMSStateUnknown
			case "invalid state":
				r.data.State = BMSState(0x12345678)
			case "not coordinated":
				a.coordinated = false
			}
			if r.CheckStateCorrect() {
				t.Fatal("unexpected state was hidden by admission waiting")
			}
		})
	}
}

func TestAdmissionDeferredStatePreservesRunningSupplyAndDisable(t *testing.T) {
	a := newAdmissionTestPolicy()
	r := admissionTestReader(a, 0)
	now := time.Now()
	r.data = admissionFrame(53000, BMSStateActive)
	observeWritten(a, 0, r.data, now.Add(-admissionMaxAge-time.Second))
	if !r.admitCommand(fsm.BMSCmdOn, now) || !r.CheckStateCorrect() {
		t.Fatal("running supply lost its heartbeat after evidence expiry")
	}
	r.enabled = false
	if r.ShouldSendOn() || r.CheckStateCorrect() {
		t.Fatal("explicit disable was bypassed")
	}
}
