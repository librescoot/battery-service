package battery

import "time"

const (
	samplingMaxSkew    = time.Second
	samplingFrameLimit = 2 * time.Second
	samplingDeadline   = 10 * time.Second
)

type acquisition struct {
	start, end, complete time.Time
	data                 BMSData
}

type samplingRound struct {
	id      uint64
	opened  time.Time
	claimed [2]bool
	samples [2]acquisition
}

type samplingReport struct {
	id             uint64
	reason         string
	skew           time.Duration
	status0, frame [2]time.Duration
}

type samplingRounds struct {
	enabled, stopped bool
	next             uint64
	round            *samplingRound
	pairedAt         time.Time
	pairedID         uint64
	report           samplingReport
}

// All round state shares the admission mutex. No round waits for a reader or
// holds this mutex across NFC I/O. A reader claims at most one read per round.
func (a *packAdmission) beginRound(now time.Time) (uint64, samplingReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.sampling.round; r != nil {
		if now.Sub(r.opened) <= samplingDeadline {
			return 0, samplingReport{}
		}
		a.cancelRoundLocked("incomplete or expired")
	}
	report := a.sampling.report
	a.sampling.report = samplingReport{}
	if !a.sampling.enabled || a.sampling.stopped || !a.slots[0].eligible || !a.slots[1].eligible || !a.needsPollLocked() {
		return 0, report
	}
	a.sampling.next++
	a.sampling.round = &samplingRound{id: a.sampling.next, opened: now}
	return a.sampling.next, report
}

func (a *packAdmission) claimRound(index int, now time.Time) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.sampling.round
	if r == nil || r.claimed[index] || now.Before(r.opened) || now.Sub(r.opened) > samplingDeadline {
		return 0
	}
	r.claimed[index] = true
	return r.id
}

func (a *packAdmission) cancelRoundLocked(reason string) {
	if r := a.sampling.round; r != nil {
		a.sampling.report = roundReport(r, reason)
		a.sampling.round = nil
	}
	a.sampling.pairedAt = time.Time{}
	a.sampling.pairedID = 0
	if a.sampling.enabled {
		for i := range a.slots {
			a.slots[i].history = nil
		}
	}
}

func roundReport(r *samplingRound, reason string) samplingReport {
	report := samplingReport{id: r.id, reason: reason}
	if !r.samples[0].start.IsZero() && !r.samples[1].start.IsZero() {
		_, report.skew = acquisitionSpan(r.samples[0], r.samples[1])
	}
	for i, sample := range r.samples {
		if !sample.start.IsZero() {
			report.status0[i] = sample.end.Sub(sample.start)
			report.frame[i] = sample.complete.Sub(sample.start)
		}
	}
	return report
}

// The union of STATUS0 intervals bounds every possible pair of acquisition
// instants, including retries. Start-time distance alone can hide a slow read.
func acquisitionSpan(a, b acquisition) (time.Time, time.Duration) {
	start, end := a.start, a.end
	if b.start.Before(start) {
		start = b.start
	}
	if b.end.After(end) {
		end = b.end
	}
	return start, end.Sub(start)
}

func (a *packAdmission) stopSampling() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sampling.stopped = true
	a.cancelRoundLocked("shutdown")
}

func (a *packAdmission) observeAcquisition(index int, id uint64, sample acquisition) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// OFF confirmation and live-supply tracking do not depend on pairing.
	a.observeLocked(index, sample.data, sample.complete)
	a.slots[index].observedRound = id
	if !a.sampling.enabled || !a.coordinated {
		return
	}
	r := a.sampling.round
	if id == 0 || r == nil || id != r.id || !r.claimed[index] {
		a.cancelRoundLocked("unpaired or late result")
		return
	}
	if !r.samples[index].complete.IsZero() {
		return
	}
	r.samples[index] = sample
	if sample.start.Before(r.opened) || sample.end.Before(sample.start) || sample.complete.Before(sample.end) ||
		sample.complete.Sub(r.opened) > samplingDeadline || sample.complete.Sub(sample.start) > samplingFrameLimit ||
		!a.slots[index].fresh(sample.complete) {
		a.cancelRoundLocked("slow or stale acquisition")
		return
	}
	other := r.samples[1-index]
	if other.complete.IsZero() {
		return
	}
	start, skew := acquisitionSpan(sample, other)
	if skew > samplingMaxSkew {
		a.cancelRoundLocked("acquisition skew")
		return
	}
	for i, s := range r.samples {
		if !a.slots[i].fresh(sample.complete) || !a.slots[i].observed.Equal(s.complete) {
			a.cancelRoundLocked("superseded or stale frame")
			return
		}
	}
	for i, s := range r.samples {
		a.slots[i].appendHistory(s.data, s.start)
	}
	a.sampling.pairedAt = start
	a.sampling.pairedID = r.id
	a.sampling.report = roundReport(r, "paired")
	a.sampling.round = nil
}
