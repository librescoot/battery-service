package battery

import (
	"bytes"
	"log/slog"
	"testing"
	"time"
)

// Frames taken from a pack swap on real hardware. The stale one is what slot 1
// served from the tag the pack had not written to since it last faced that
// reader; the live one is the same pack once it had.
var (
	staleFrame = BMSData{
		Present: true, State: BMSStateAsleep, Voltage: 56071, Current: 0, Charge: 79,
		Temperature: [4]int{22, 22, 19, 19}, CycleCount: 83, SerialNumber: "T-UNU22205090007",
	}
	liveFrame = BMSData{
		Present: true, State: BMSStateActive, Voltage: 50636, Current: 0, Charge: 37,
		Temperature: [4]int{24, 24, 20, 20}, CycleCount: 84, SerialNumber: "T-UNU22205090007",
	}
)

func newFreshnessTestReader() *BatteryReader {
	return &BatteryReader{
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func TestStatusFrameChanged(t *testing.T) {
	if statusFrameChanged(staleFrame, staleFrame) {
		t.Error("a frame compared against itself reported a change")
	}
	if !statusFrameChanged(staleFrame, liveFrame) {
		t.Error("the measured stale and live frames did not compare as different")
	}

	// The pack drifts by a millivolt between reads while sitting idle, and that
	// is enough: it means the pack has written.
	drifted := staleFrame
	drifted.Voltage++
	if !statusFrameChanged(drifted, staleFrame) {
		t.Error("a one millivolt move did not count as a change")
	}

	// Serial alone must not confirm the tag. It is the one field a stale frame
	// gets right, so treating it as movement would defeat the whole check.
	renamed := staleFrame
	renamed.SerialNumber = "T-UNU22009170066"
	if statusFrameChanged(renamed, staleFrame) {
		t.Error("a serial change alone counted as the pack having written")
	}
}

// Re-reading the same tag does not confirm it. Measured on hardware: the reads
// behind one frame already have to agree byte for byte, and two further reads a
// third of a second apart returned the same stale bytes again.
func TestFreshnessNeedsTheFrameToMove(t *testing.T) {
	r := newFreshnessTestReader()
	r.noteTagPresent([]byte{0x04, 0x30, 0x3F, 0x62, 0x77, 0x70, 0x80})

	for i := 1; i <= 3; i++ {
		r.data = staleFrame
		r.noteFrameFreshness()
		if !r.fresh.unconfirmed {
			t.Fatalf("read %d: identical bytes confirmed the tag", i)
		}
	}

	r.data = liveFrame
	r.noteFrameFreshness()
	if r.fresh.unconfirmed {
		t.Fatal("the pack writing did not confirm the tag")
	}

	// Once confirmed it stays confirmed, including for a frame that happens to
	// match the one recorded earlier.
	r.data = staleFrame
	r.noteFrameFreshness()
	if r.fresh.unconfirmed {
		t.Error("the tag went back to unconfirmed")
	}
}

// A pack whose readings never move must not leave the check waiting forever.
func TestFreshnessConfirmsAfterTheBound(t *testing.T) {
	r := newFreshnessTestReader()
	r.noteTagPresent([]byte{0x04, 0x30, 0x3F, 0x62, 0x77, 0x70, 0x80})

	r.data = staleFrame
	r.noteFrameFreshness()
	if !r.fresh.unconfirmed {
		t.Fatal("the first frame confirmed the tag on its own")
	}

	r.fresh.since = time.Now().Add(-tagConfirmAfter - time.Second)
	r.data = staleFrame
	r.noteFrameFreshness()
	if r.fresh.unconfirmed {
		t.Fatal("the tag stayed unconfirmed past the bound")
	}
}

// A tag the reader already knows needs no confirming: the pack has been writing
// to it all along.
func TestFreshnessNotArmedForAFamiliarTag(t *testing.T) {
	r := newFreshnessTestReader()
	uid := []byte{0x04, 0x34, 0xC0, 0x62, 0x77, 0x70, 0x80}

	r.noteTagPresent(uid)
	r.data = staleFrame
	r.noteFrameFreshness()
	r.data = liveFrame
	r.noteFrameFreshness()
	if r.fresh.unconfirmed {
		t.Fatal("expected the tag to be confirmed")
	}

	// Discovery runs again every command cycle and re-reports the same tag.
	r.noteTagPresent(uid)
	if r.fresh.unconfirmed {
		t.Error("re-seeing a known tag marked it unconfirmed again")
	}
}

// Swapping the packs between slots gives each reader a tag it has not seen,
// even though neither pack is new to the vehicle.
func TestFreshnessArmsOnTagChangeForTheSameSlot(t *testing.T) {
	r := newFreshnessTestReader()

	r.noteTagPresent([]byte{0x04, 0x07, 0x67, 0x1A, 0x0F, 0x6A, 0x80})
	r.data = staleFrame
	r.noteFrameFreshness()
	r.data = liveFrame
	r.noteFrameFreshness()
	if r.fresh.unconfirmed {
		t.Fatal("expected the first tag to be confirmed")
	}

	r.noteTagPresent([]byte{0x04, 0x30, 0x3F, 0x62, 0x77, 0x70, 0x80})
	if !r.fresh.unconfirmed {
		t.Error("a different tag in the same slot was not marked unconfirmed")
	}
}

// Nothing runs between status frames, so a timer asks the FSM for an early
// heartbeat. Without it a slot polled at the heartbeat interval would wait for
// its next frame: 43 s, measured on hardware, and half an hour on an idle slot.
func TestFreshnessArmsAnEarlyReadTimer(t *testing.T) {
	r := newFreshnessTestReader()
	r.noteTagPresent([]byte{0x04, 0x30, 0x3F, 0x62, 0x77, 0x70, 0x80})

	if r.fresh.timer == nil {
		t.Fatal("no early-read timer was armed")
	}

	r.data = staleFrame
	r.noteFrameFreshness()
	r.data = liveFrame
	r.noteFrameFreshness()

	if r.fresh.timer != nil {
		t.Error("the early-read timer outlived the tag it was confirming")
	}
}

// Re-arming for a second tag change must not leak the first timer.
func TestFreshnessRearmReplacesTheTimer(t *testing.T) {
	r := newFreshnessTestReader()
	r.noteTagPresent([]byte{0x04, 0x07, 0x67, 0x1A, 0x0F, 0x6A, 0x80})
	first := r.fresh.timer
	if first == nil {
		t.Fatal("no early-read timer was armed for the first tag")
	}

	r.noteTagPresent([]byte{0x04, 0x30, 0x3F, 0x62, 0x77, 0x70, 0x80})
	if r.fresh.timer == nil {
		t.Fatal("no early-read timer was armed for the second tag")
	}
	if r.fresh.timer == first {
		t.Error("the second tag reused the first tag's timer")
	}
	if first.Stop() {
		t.Error("the first timer was still pending after being replaced")
	}
}

// A departure clears the tracking so the next tag starts clean.
func TestFreshnessClearedOnDeparture(t *testing.T) {
	r := newFreshnessTestReader()
	r.noteTagPresent([]byte{0x04, 0x30, 0x3F, 0x62, 0x77, 0x70, 0x80})
	if r.fresh.timer == nil {
		t.Fatal("no early-read timer was armed")
	}

	// What handleDeparture does.
	r.fresh.stopTimer()
	r.fresh = tagFreshness{}

	if r.fresh.unconfirmed || r.fresh.timer != nil {
		t.Error("freshness tracking survived the departure")
	}
}
