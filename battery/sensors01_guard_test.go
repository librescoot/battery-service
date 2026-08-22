package battery

import (
	"bytes"
	"log/slog"
	"testing"
	"time"
)

func newSensors01TestReader() *BatteryReader {
	return &BatteryReader{
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func TestSensors01Suspect(t *testing.T) {
	recent := 10 * time.Second
	stale := sensors01PreviousMaxAge + time.Second

	warm := sensors01Sample{valid: true, temperature: [2]int{30, 30}}
	nearHot := sensors01Sample{valid: true, temperature: [2]int{sensors01HotCredibleFrom, sensors01HotCredibleFrom}}
	nearCold := sensors01Sample{valid: true, temperature: [2]int{sensors01ColdCredibleTo, sensors01ColdCredibleTo}}

	cases := []struct {
		name    string
		t0, t1  int
		prev    sensors01Sample
		prevAge time.Duration
		want    bool
		wantWhy string
	}{
		{"ordinary reading", 30, 30, warm, recent, false, "nothing near an extreme"},
		{"one channel hot, other ordinary", sensors01ExtremeHot, 30, warm, recent, false,
			"one sensor alone is not the signature"},
		{"one channel cold, other ordinary", sensors01ExtremeCold, 30, warm, recent, false,
			"one sensor alone is not the signature"},
		{"hot and cold extremes together", sensors01ExtremeHot, sensors01ExtremeCold, warm, recent, false,
			"opposite extremes are not a restart"},

		{"both at hot extreme from warm", sensors01ExtremeHot, sensors01ExtremeHot, warm, recent, true,
			"a pack cannot jump from 30 to the extreme in one interval"},
		{"both at cold extreme from warm", sensors01ExtremeCold, sensors01ExtremeCold, warm, recent, true,
			"same, at the cold end"},

		{"both at hot extreme with a hot predecessor", sensors01ExtremeHot, sensors01ExtremeHot, nearHot, recent, false,
			"a real excursion arrives gradually and must be believed"},
		{"both at cold extreme with a cold predecessor", sensors01ExtremeCold, sensors01ExtremeCold, nearCold, recent, false,
			"same, at the cold end"},

		{"both at hot extreme, no predecessor", sensors01ExtremeHot, sensors01ExtremeHot, sensors01Sample{}, recent, true,
			"first read after a pack appears is the likeliest moment to catch this"},
		{"both at hot extreme, stale predecessor", sensors01ExtremeHot, sensors01ExtremeHot, nearHot, stale, true,
			"too old to say anything, so it counts as absent"},

		{"hot extreme just below the credible threshold", sensors01ExtremeHot, sensors01ExtremeHot,
			sensors01Sample{valid: true, temperature: [2]int{sensors01HotCredibleFrom - 1, sensors01HotCredibleFrom - 1}}, recent, true, ""},
		{"cold extreme just above the credible threshold", sensors01ExtremeCold, sensors01ExtremeCold,
			sensors01Sample{valid: true, temperature: [2]int{sensors01ColdCredibleTo + 1, sensors01ColdCredibleTo + 1}}, recent, true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sensors01Suspect(tc.t0, tc.t1, tc.prev, tc.prevAge)
			if got != tc.want {
				t.Errorf("sensors01Suspect(%d, %d, %+v, %s) = %t, want %t (%s)",
					tc.t0, tc.t1, tc.prev, tc.prevAge, got, tc.want, tc.wantWhy)
			}
		})
	}
}

// The frame that started this: both sensors at the hot extreme and the voltage
// reading a fraction of true, with the other two sensors correct.
func TestSensors01GuardHoldsLastGoodOnArtifact(t *testing.T) {
	r := newSensors01TestReader()
	now := time.Now()

	r.data.Temperature = [4]int{27, 27, 24, 24}
	r.data.Voltage = 57805
	r.data.Current = -40
	if !r.applySensors01Guard(now) {
		t.Fatal("an ordinary sample was treated as suspect")
	}

	r.data.Temperature = [4]int{sensors01ExtremeHot, sensors01ExtremeHot, 25, 25}
	r.data.Voltage = 21091
	r.data.Current = 0
	if !r.applySensors01Guard(now.Add(30 * time.Second)) {
		t.Fatal("guard reported no usable channel 0/1 value despite having one to substitute")
	}

	if r.data.Temperature[0] != 27 || r.data.Temperature[1] != 27 {
		t.Errorf("temperatures = %v, want the last trusted 27/27", r.data.Temperature)
	}
	if r.data.Temperature[2] != 25 || r.data.Temperature[3] != 25 {
		t.Errorf("channels 2/3 = %v, want them untouched at 25/25", r.data.Temperature)
	}
	if r.data.Voltage != 57805 {
		t.Errorf("voltage = %d, want the last trusted 57805", r.data.Voltage)
	}
	if r.data.Current != -40 {
		t.Errorf("current = %d, want the last trusted -40", r.data.Current)
	}
	if r.sensors01Artifacts != 1 {
		t.Errorf("artifact counter = %d, want 1", r.sensors01Artifacts)
	}

	r.updateTemperatureState(true)
	if r.data.TemperatureState != BMSTemperatureStateIdeal {
		t.Errorf("temperature state = %d, want ideal", r.data.TemperatureState)
	}
}

// The artifact must not become the reference, or the next real sample looks
// like the anomaly and the substitution latches.
func TestSensors01GuardDoesNotAdoptArtifactAsReference(t *testing.T) {
	r := newSensors01TestReader()
	now := time.Now()

	r.data.Temperature = [4]int{27, 27, 24, 24}
	r.data.Voltage = 57805
	r.applySensors01Guard(now)

	r.data.Temperature = [4]int{sensors01ExtremeHot, sensors01ExtremeHot, 25, 25}
	r.data.Voltage = 21091
	r.applySensors01Guard(now.Add(10 * time.Second))

	r.data.Temperature = [4]int{29, 29, 26, 26}
	r.data.Voltage = 57803
	if !r.applySensors01Guard(now.Add(20 * time.Second)) {
		t.Fatal("a recovered sample was treated as suspect")
	}
	if r.data.Voltage != 57803 || r.data.Temperature[0] != 29 {
		t.Errorf("a recovered sample was overwritten: temp=%v voltage=%d", r.data.Temperature, r.data.Voltage)
	}
	if r.lastGoodSensors01.voltage != 57803 {
		t.Errorf("reference voltage = %d, want the recovered 57803", r.lastGoodSensors01.voltage)
	}
	if r.sensors01Artifacts != 1 {
		t.Errorf("artifact counter = %d, want 1", r.sensors01Artifacts)
	}
}

// First read after a pack appears: nothing has been believed yet, so there is
// nothing to substitute. The state must come off sensors 2 and 3 rather than
// off an extreme.
func TestSensors01GuardWithoutReferenceFallsBackToOtherSensors(t *testing.T) {
	r := newSensors01TestReader()

	r.data.Temperature = [4]int{sensors01ExtremeHot, sensors01ExtremeHot, 25, 25}
	r.data.Voltage = 21091
	if r.applySensors01Guard(time.Now()) {
		t.Fatal("guard claimed channels 0/1 were usable with nothing to substitute")
	}

	// Left raw so the artifact stays visible downstream.
	if r.data.Temperature[0] != sensors01ExtremeHot {
		t.Errorf("temperature[0] = %d, want the raw %d", r.data.Temperature[0], sensors01ExtremeHot)
	}

	r.updateTemperatureState(false)
	if r.data.TemperatureState != BMSTemperatureStateIdeal {
		t.Errorf("temperature state = %d, want ideal from channels 2/3", r.data.TemperatureState)
	}
}

// A pack that really is that hot keeps reporting it. Once a sample at the
// extreme has been believed it becomes the reference, and the next one must be
// believed too.
func TestSensors01GuardBelievesSustainedExtreme(t *testing.T) {
	r := newSensors01TestReader()
	now := time.Now()

	r.data.Temperature = [4]int{85, 85, 60, 60}
	r.applySensors01Guard(now)

	for i := 1; i <= 3; i++ {
		r.data.Temperature = [4]int{sensors01ExtremeHot, sensors01ExtremeHot, 70, 70}
		if !r.applySensors01Guard(now.Add(time.Duration(i) * 10 * time.Second)) {
			t.Fatalf("sample %d: a sustained extreme reading was rejected", i)
		}
		if r.data.Temperature[0] != sensors01ExtremeHot {
			t.Fatalf("sample %d: a believed extreme reading was overwritten with %d", i, r.data.Temperature[0])
		}
	}
	if r.sensors01Artifacts != 0 {
		t.Errorf("artifact counter = %d, want 0 for a real excursion", r.sensors01Artifacts)
	}

	r.updateTemperatureState(true)
	if r.data.TemperatureState != BMSTemperatureStateHot {
		t.Errorf("temperature state = %d, want hot", r.data.TemperatureState)
	}
}

func TestSensors01GuardColdExtreme(t *testing.T) {
	r := newSensors01TestReader()
	now := time.Now()

	r.data.Temperature = [4]int{5, 5, 6, 6}
	r.data.Voltage = 51000
	r.applySensors01Guard(now)

	r.data.Temperature = [4]int{sensors01ExtremeCold, sensors01ExtremeCold, 6, 6}
	r.data.Voltage = 20000
	if !r.applySensors01Guard(now.Add(10 * time.Second)) {
		t.Fatal("guard reported no usable value despite having one to substitute")
	}
	if r.data.Temperature[0] != 5 {
		t.Errorf("temperature[0] = %d, want the last trusted 5", r.data.Temperature[0])
	}
	if r.data.Voltage != 51000 {
		t.Errorf("voltage = %d, want the last trusted 51000", r.data.Voltage)
	}

	r.updateTemperatureState(true)
	if r.data.TemperatureState != BMSTemperatureStateIdeal {
		t.Errorf("temperature state = %d, want ideal", r.data.TemperatureState)
	}
}

// A slot polled less often than the predecessor lifetime never has a usable
// reference, so it substitutes rather than accepting an extreme reading.
func TestSensors01GuardStalePreviousSample(t *testing.T) {
	r := newSensors01TestReader()
	now := time.Now()

	r.data.Temperature = [4]int{90, 90, 60, 60}
	r.applySensors01Guard(now)

	r.data.Temperature = [4]int{sensors01ExtremeHot, sensors01ExtremeHot, 61, 61}
	r.applySensors01Guard(now.Add(30 * time.Minute))

	if r.sensors01Artifacts != 1 {
		t.Errorf("artifact counter = %d, want 1: a 30 minute old predecessor says nothing", r.sensors01Artifacts)
	}
	if r.data.Temperature[0] != 90 {
		t.Errorf("temperature[0] = %d, want the last trusted 90", r.data.Temperature[0])
	}
}
