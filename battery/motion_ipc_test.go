package battery

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const testMotionSource = "cd0ed924a45d4af18b65395fd632bd11"

func motionTestPayload(at time.Time, sequence uint64, changes map[string]any) string {
	fields := map[string]any{"version": 1, "source": testMotionSource, "sequence": sequence, "valid": true, "observed_at": at.Format(time.RFC3339Nano), "rpm": 0, "raw_speed": 0, "throttle_on": false}
	for k, v := range changes {
		if v == nil {
			delete(fields, k)
		} else {
			fields[k] = v
		}
	}
	data, _ := json.Marshal(fields)
	return string(data)
}

func TestMotionIPCRequiredFieldsAndExpiry(t *testing.T) {
	now := admissionTestTime
	cases := []struct {
		name              string
		changes           map[string]any
		accepted, stopped bool
	}{
		{"zero values", nil, true, true},
		{"rpm", map[string]any{"rpm": 1}, true, false},
		{"raw speed", map[string]any{"raw_speed": 1}, true, false},
		{"throttle", map[string]any{"throttle_on": true}, true, false},
		{"future", map[string]any{"observed_at": now.Add(time.Nanosecond).Format(time.RFC3339Nano)}, false, false},
		{"expired", map[string]any{"observed_at": now.Add(-stoppedMotionMaxAge - time.Nanosecond).Format(time.RFC3339Nano)}, false, false},
		{"age boundary", map[string]any{"observed_at": now.Add(-stoppedMotionMaxAge).Format(time.RFC3339Nano)}, true, true},
		{"version", map[string]any{"version": 2}, false, false},
		{"source", map[string]any{"source": "abc"}, false, false},
		{"source case", map[string]any{"source": strings.ToUpper(testMotionSource)}, false, false},
		{"zero seq", map[string]any{"sequence": 0}, false, false},
		{"speed range", map[string]any{"raw_speed": 256}, false, false},
		{"rpm range", map[string]any{"rpm": 65536}, false, false},
		{"negative rpm", map[string]any{"rpm": -1}, false, false},
		{"string throttle", map[string]any{"throttle_on": "false"}, false, false},
	}
	for _, field := range []string{"version", "source", "sequence", "valid", "observed_at", "rpm", "raw_speed", "throttle_on"} {
		cases = append(cases, struct {
			name              string
			changes           map[string]any
			accepted, stopped bool
		}{"missing " + field, map[string]any{field: nil}, false, false})
		cases = append(cases, struct {
			name              string
			changes           map[string]any
			accepted, stopped bool
		}{"null " + field, map[string]any{field: json.RawMessage("null")}, false, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := samplingPolicy()
			got := a.receiveMotionEvidence(motionTestPayload(now, 1, tc.changes), now)
			if got != tc.accepted || a.motion.stopped != tc.stopped {
				t.Fatalf("accepted=%v stopped=%v", got, a.motion.stopped)
			}
		})
	}
	for _, payload := range []string{"{", "null", strings.Repeat(" ", 1025)} {
		a := samplingPolicy()
		if a.receiveMotionEvidence(payload, now) {
			t.Fatal("malformed payload accepted")
		}
	}
}

func TestMotionIPCDiscontinuityRestartsQualification(t *testing.T) {
	for _, kind := range []string{"continuous", "lost pulse", "duplicate", "reordered", "source restart", "invalid", "malformed", "reconnect"} {
		t.Run(kind, func(t *testing.T) {
			a, now := readyStoppedPolicy(t)
			a.motion.source = testMotionSource
			a.motion.sequence = 10
			at := now.Add(time.Millisecond)
			seq := uint64(11)
			changes := map[string]any{}
			switch kind {
			case "lost pulse":
				seq = 12
			case "duplicate":
				seq = 10
			case "reordered":
				seq = 9
			case "source restart":
				changes["source"] = "00000000000000000000000000000001"
			case "invalid":
				changes = map[string]any{"valid": false, "reason": "rx-overflow", "observed_at": nil, "rpm": nil, "raw_speed": nil, "throttle_on": nil}
			case "reconnect":
				a.resetMotionStream()
			}
			payload := motionTestPayload(at, seq, changes)
			if kind == "malformed" {
				payload = "{"
			}
			a.receiveMotionEvidence(payload, at)
			requireAdmission(t, a, 1, at, kind == "continuous")
			requireAdmission(t, a, 0, at, true)
			if kind != "continuous" && a.slots[1].held {
				t.Fatal("invalid evidence reserved new supply")
			}
		})
	}
}

func TestMotionIPCPreservesAcquisitionTimeAndUnchangedRefreshes(t *testing.T) {
	a := samplingPolicy()
	now := admissionTestTime
	for i := 0; i <= 60; i++ {
		at := now.Add(time.Duration(i) * 100 * time.Millisecond)
		if !a.receiveMotionEvidence(motionTestPayload(at, uint64(i+1), nil), at.Add(100*time.Millisecond)) {
			t.Fatal("fresh unchanged value rejected")
		}
	}
	if !a.motion.since.Equal(now) || !a.motion.at.Equal(now.Add(6*time.Second)) {
		t.Fatal("delivery time replaced observation time")
	}
	// A post-pair delivery cannot substitute for an acquisition before completion.
	a, now = readyStoppedPolicy(t)
	a.motion.source = testMotionSource
	a.motion.sequence = 100
	a.motion.at = now.Add(-200 * time.Millisecond)
	a.receiveMotionEvidence(motionTestPayload(now.Add(-time.Millisecond), 101, nil), now.Add(time.Millisecond))
	requireAdmission(t, a, 1, now.Add(time.Millisecond), false)
}

func TestMotionWireFixtures(t *testing.T) {
	valid, err := os.ReadFile("testdata/motion-valid.json")
	if err != nil {
		t.Fatal(err)
	}
	invalid, err := os.ReadFile("testdata/motion-invalid.json")
	if err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339Nano, "2026-10-17T10:00:00.123456789Z")
	a := samplingPolicy()
	if !a.receiveMotionEvidence(string(valid), now) || !a.motion.stopped {
		t.Fatal("producer fixture rejected")
	}
	if a.receiveMotionEvidence(string(invalid), now) || a.motion.stopped || a.motion.sequence != 43 {
		t.Fatal("discriminated invalidation not consumed")
	}
}

func TestMotionSubscriberCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, _ := readyStoppedPolicy(t)
	s := &Service{ctx: ctx, admission: a}
	s.motionWG.Add(1)
	s.runMotionSubscriber()
	s.motionWG.Wait()
	if a.motion.stopped || a.motion.source != "" {
		t.Fatal("cancelled subscriber retained evidence")
	}
}

func TestMotionIPCIntegration(t *testing.T) {
	producer := os.Getenv("ECU_MOTION_TEST_BINARY")
	if producer == "" {
		t.Skip("set ECU_MOTION_TEST_BINARY to ecu-service's compiled test binary")
	}
	server, err := exec.LookPath("redis-server")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	socket := filepath.Join(t.TempDir(), "redis.sock")
	cmd := exec.CommandContext(ctx, server, "--port", "0", "--save", "", "--appendonly", "no", "--unixsocket", socket, "--unixsocketperm", "700", "--loglevel", "warning")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	client := redis.NewClient(&redis.Options{Network: "unix", Addr: socket, MaxRetries: -1})
	defer client.Close()
	wait := func(label string, fn func() bool) {
		t.Helper()
		until := time.Now().Add(3 * time.Second)
		for !fn() {
			if time.Now().After(until) {
				t.Fatalf("timeout: %s", label)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait("Redis", func() bool { return client.Ping(ctx).Err() == nil })
	a := samplingPolicy()
	a.motion.enabled = true
	a.setVehicle(VehicleStateReadyToDrive)
	s := &Service{ctx: ctx, redis: client, admission: a, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.motionWG.Add(1)
	go s.runMotionSubscriber()
	defer func() { cancel(); s.motionWG.Wait() }()
	wait("subscription", func() bool {
		n, err := client.PubSubNumSub(ctx, motionEvidenceChannel).Result()
		return err == nil && n[motionEvidenceChannel] == 1
	})
	p := exec.CommandContext(ctx, producer, "-test.run=^TestMotionIPCProducerFixture$", "-test.timeout=20s")
	p.Env = append(os.Environ(), "MOTION_TEST_REDIS_SOCKET="+socket)
	p.Stdout, p.Stderr = os.Stdout, os.Stderr
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Process.Kill(); _ = p.Wait() }()
	wait("kernel-timestamped ECU evidence", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.motion.stopped })
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(admissionPollInterval)
		}
		deliverPair(t, a, time.Now(), 52001+uint(i%2))
	}
	if ok, _ := a.requestOn(1, time.Now(), 1000); ok {
		t.Fatal("ON allowed before post-pair observation")
	}
	wait("qualified admission", func() bool { ok, _ := a.requestOn(1, time.Now(), 1000); return ok })
	// Kill only this isolated Redis subscriber. Running supply must survive;
	// reconnection must not carry over the stopped window or release reservations.
	if err := client.ClientKillByFilter(ctx, "TYPE", "pubsub").Err(); err != nil {
		t.Fatal(err)
	}
	wait("disconnect invalidation", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.motion.since.IsZero() })
	if ok, reason := a.requestOn(0, time.Now(), 1000); !ok {
		t.Fatalf("running supply stopped: %s", reason)
	}
	a.mu.Lock()
	held := a.slots[1].held
	a.mu.Unlock()
	if !held {
		t.Fatal("disconnect released ON reservation")
	}
}
