package battery

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/librescoot/pn7150"
)

// Requires exclusive access to this reader, with no battery-service or other
// process using it. No BMS commands are sent and Redis publication is mocked.
func TestHardwareDiscoveryObservations(t *testing.T) {
	device := os.Getenv("NFC_HARDWARE_TEST_DEVICE")
	if device == "" {
		t.Skip("set NFC_HARDWARE_TEST_DEVICE with exclusive reader access")
	}
	expected := os.Getenv("NFC_HARDWARE_TEST_EXPECTED")
	if expected != "present" && expected != "absent" {
		t.Fatal("NFC_HARDWARE_TEST_EXPECTED must be present or absent")
	}
	r, published := absenceReader(t, 1)
	controller, err := hal.NewPN7150(device, func(level hal.LogLevel, message string) { t.Logf("NCI level=%d %s", level, message) }, nil, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	r.hal = controller
	if err := controller.Initialize(); err != nil {
		t.Fatal(err)
	}
	cycles := 1
	if expected == "present" {
		cycles = 20
	}
	quiet, pending, found := 0, 0, 0
	for cycle := 0; cycle < cycles; cycle++ {
		r.nfcMu.Lock()
		started := r.startDiscovery()
		r.nfcMu.Unlock()
		if !started {
			t.Fatal("discovery did not start")
		}
		begin := time.Now()
		seen := false
		for time.Since(begin) < 10*time.Second {
			result, err := controller.PollDiscovery()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("cycle=%d elapsed=%s kind=%d cached=%t tags=%d", cycle, time.Since(begin), result.Kind, result.Cached, len(result.Tags))
			switch result.Kind {
			case hal.DiscoveryQuiet:
				quiet++
			case hal.DiscoveryTags:
				found++
				seen = true
			default:
				pending++
			}
			r.nfcMu.Lock()
			r.processTagArrivalPoll(result, nil)
			r.nfcMu.Unlock()
			if seen {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if seen != (expected == "present") {
			t.Fatalf("expected %s, tag detected=%t", expected, seen)
		}
		if !r.data.Present || published.fields["present"] != "true" {
			t.Fatal("polling cleared the cached battery")
		}
		if !seen {
			r.FinishDiscovery()
			if r.data.Present || published.fields["present"] != "false" {
				t.Fatal("completed empty hardware search retained phantom battery")
			}
		}
	}
	if expected == "present" {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if r.PollForTagArrival(ctx) {
			t.Fatal("cancelled hardware poll continued")
		}
	}
	t.Logf("RESULT cycles=%d quiet=%d pending=%d detections=%d expected=%s", cycles, quiet, pending, found, expected)
}
