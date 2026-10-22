package battery

import (
	"fmt"
	"testing"
	"time"
)

func TestAdmissionUnconfirmedTagCannotBorrowFreshAcquisitions(t *testing.T) {
	for _, confirmedPeer := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirmedPeer=%t", confirmedPeer), func(t *testing.T) {
			testUnconfirmedTagWithCachedVoltage(t, confirmedPeer)
		})
	}
}

func testUnconfirmedTagWithCachedVoltage(t *testing.T, confirmedPeer bool) {
	t.Helper()
	s := newAdmissionSimulation(t, "experimental")
	s.pack = [2]BMSData{admissionFrame(53000, BMSStateActive), admissionFrame(49000, BMSStateIdle)}
	a := s.a
	a.tag(0, []byte{1})
	a.tag(1, []byte{2})
	if confirmedPeer {
		observeWritten(a, 0, s.pack[0], admissionTestTime.Add(-time.Second))
	}
	for step := 0; step < 45; step++ {
		at := admissionTestTime.Add(time.Duration(step) * admissionPollInterval)
		id, _ := a.beginRound(at)
		for slot := range a.slots {
			a.claimRound(slot, at)
		}
		for slot := range a.slots {
			// Tag memory reports a compatible voltage, while the newly inserted
			// pack may physically differ. Only the cached observations are fed in.
			a.observeAcquisition(slot, id, roundSample(slot, at, at.Add(100*time.Millisecond), at.Add(200*time.Millisecond), 53000))
		}
		s.now = at.Add(200 * time.Millisecond)
		s.command(1, true)
		if s.pack[1].State != BMSStateIdle {
			t.Fatal("cached compatible tag values activated a physically mismatched pack")
		}
	}
}

func TestAdmissionConstantReadingsStartAndJoin(t *testing.T) {
	for _, mode := range []string{"single", "dual", "experimental"} {
		for _, first := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/first%d", mode, first), func(t *testing.T) {
				s := newAdmissionSimulation(t, mode)
				s.insert(0, 56500, 83, BMSStateIdle)
				s.insert(1, 56490, 81, BMSStateIdle)
				for step := 0; step < 45; step++ {
					s.observe()
					s.command(first, true)
					s.command(1-first, true)
				}
				if s.pack[0].State != BMSStateActive || (mode != "single" && s.pack[1].State != BMSStateActive) {
					s.fail("constant successful readings did not activate the eligible packs")
				}
			})
		}
	}
}

func TestAdmissionConstantReadingsExpireWithoutAcquisition(t *testing.T) {
	a := newAdmissionTestPolicy()
	observeWritten(a, 0, admissionFrame(56500, BMSStateActive), admissionTestTime)
	observeWritten(a, 1, admissionFrame(56490, BMSStateIdle), admissionTestTime)
	for step := 0; step < 45; step++ {
		now := admissionTestTime.Add(time.Duration(step) * admissionPollInterval)
		a.observe(0, admissionFrame(56500, BMSStateActive), now)
		a.observe(1, admissionFrame(56490, BMSStateIdle), now)
	}
	now := admissionTestTime.Add(44 * admissionPollInterval)
	if !a.slots[0].fresh(now) || !a.slots[1].stable(now) {
		t.Fatal("unchanged successful acquisitions expired")
	}
	requireAdmission(t, a, 1, now.Add(admissionMaxAge+time.Nanosecond), false)
	a.invalidate(1)
	requireAdmission(t, a, 1, now, false)
}

func TestAdmissionConstantIdleDoesNotReleaseUncertainCommands(t *testing.T) {
	for _, off := range []bool{false, true} {
		a := newAdmissionTestPolicy()
		for i := range a.slots {
			observeWritten(a, i, admissionFrame(53000, BMSStateIdle), admissionTestTime)
		}
		requireAdmission(t, a, 0, admissionTestTime, true)
		if off {
			a.offAttempt(0, admissionTestTime.Add(time.Second))
		}
		for step := 1; step < 45; step++ {
			now := admissionTestTime.Add(time.Duration(step) * admissionPollInterval)
			for i := range a.slots {
				a.observe(i, admissionFrame(53000, BMSStateIdle), now)
			}
			if !a.slots[0].held || (off && !a.slots[0].offPending) {
				t.Fatal("unchanged idle payload released an uncertain command")
			}
			requireAdmission(t, a, 1, now, false)
		}
	}
}
