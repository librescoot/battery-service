package battery

import (
	"io"
	"log/slog"
	"testing"
)

func newFaultTestReader() *BatteryReader {
	r := &BatteryReader{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r.initializeFaultManagement()
	return r
}

// The NFC reader fault must arm exactly once across the reinit retry loop, so
// the 30s debounce isn't restarted every ~2s and can actually elapse to flag a
// dead reader as not-present.
func TestNFCReaderErrorEdgeTriggered(t *testing.T) {
	r := newFaultTestReader()
	st := r.faultStates[BMSFaultNFCReaderError]

	if r.nfcReaderErrorRaised || st.PendingSet {
		t.Fatalf("precondition: raised=%t pendingSet=%t, want both false", r.nfcReaderErrorRaised, st.PendingSet)
	}

	r.raiseNFCReaderError()
	if !r.nfcReaderErrorRaised || !st.PendingSet {
		t.Fatalf("after first raise: raised=%t pendingSet=%t, want both true", r.nfcReaderErrorRaised, st.PendingSet)
	}
	firstTimer := st.SetTimer
	if firstTimer == nil {
		t.Fatal("after first raise: debounce timer not armed")
	}

	// Re-raising during the retry loop must not restart the debounce timer.
	r.raiseNFCReaderError()
	if st.SetTimer != firstTimer {
		t.Error("second raise restarted the debounce timer; it must be idempotent")
	}

	// A successful init clears the pending fault and disarms the timer.
	r.clearNFCReaderError()
	if r.nfcReaderErrorRaised || st.PendingSet || st.SetTimer != nil {
		t.Errorf("after clear: raised=%t pendingSet=%t timer!=nil=%t, want all cleared",
			r.nfcReaderErrorRaised, st.PendingSet, st.SetTimer != nil)
	}
}
