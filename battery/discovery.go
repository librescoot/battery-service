package battery

import "github.com/librescoot/pn7150"

type discoveryEvidence struct {
	quiet     bool
	uncertain bool
}

// FinishDiscovery runs only when the existing ten-second discovery watchdog
// expires, after cancelling its polling loop. That search window spans several
// slow RF cycles; a single 250ms notification wait cannot establish absence.
func (r *BatteryReader) FinishDiscovery() {
	r.nfcMu.Lock()
	defer r.nfcMu.Unlock()
	if !r.discovery.quiet || r.discovery.uncertain || !r.hasCachedBattery() {
		return
	}
	// Drain a notification that may have arrived at the watchdog boundary
	// before deciding the search found nothing. Recovery follows either way.
	result, err := r.hal.PollDiscovery()
	r.finishDiscovery(result, err)
}

func (r *BatteryReader) hasCachedBattery() bool {
	// previousData remains present if the departure's Redis transaction failed.
	return r.previousTagPresent || r.data.Present || r.previousData.Present
}

func (r *BatteryReader) finishDiscovery(final hal.DiscoveryResult, err error) {
	if !r.discovery.quiet || r.discovery.uncertain || !r.hasCachedBattery() {
		return
	}
	if err != nil || final.Kind != hal.DiscoveryQuiet {
		r.discovery.uncertain = true
		return
	}
	r.previousTagPresent = false
	r.tagsDiscovered = false
	r.handleDeparture("no tag during completed discovery search")
}
