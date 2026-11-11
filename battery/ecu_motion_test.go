package battery

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type motionRedisHook struct {
	updatedAt string
}

func (h *motionRedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unexpected Redis connection")
	}
}

func (h *motionRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *motionRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		switch c := cmd.(type) {
		case *redis.StringCmd:
			c.SetVal("on")
		case *redis.SliceCmd:
			c.SetVal([]any{"0", "0", "off", h.updatedAt})
		default:
			return errors.New("unexpected Redis command")
		}
		return nil
	}
}

func TestStaleECUHashCannotQualifyStandstill(t *testing.T) {
	now := time.Unix(1700000000, 0)
	hook := &motionRedisHook{updatedAt: strconv.FormatInt(now.UnixMilli(), 10)}
	client := redis.NewClient(&redis.Options{Addr: "unused"})
	client.AddHook(hook)
	t.Cleanup(func() { _ = client.Close() })
	a := &packAdmission{}
	a.setVehicleAt(VehicleStateReadyToDrive, now)
	s := &Service{ctx: context.Background(), redis: client, admission: a}
	for _, delta := range []time.Duration{0, 3 * time.Second, 6 * time.Second} {
		s.sampleECUMotion(now.Add(delta))
	}
	if a.stationaryLocked(now.Add(6 * time.Second)) {
		t.Fatal("unchanged stale ECU values qualified as standstill")
	}
	for _, delta := range []time.Duration{9 * time.Second, 12 * time.Second, 15 * time.Second} {
		hook.updatedAt = strconv.FormatInt(now.Add(delta).UnixMilli(), 10)
		s.sampleECUMotion(now.Add(delta))
	}
	if !a.stationaryLocked(now.Add(15 * time.Second)) {
		t.Fatal("fresh stopped samples did not qualify")
	}
}

func TestECUMotionFresh(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name  string
		value any
		want  bool
	}{
		{"current", strconv.FormatInt(now.UnixMilli(), 10), true},
		{"one second old", strconv.FormatInt(now.Add(-time.Second).UnixMilli(), 10), true},
		{"at limit", strconv.FormatInt(now.Add(-ecuMotionMaxAge).UnixMilli(), 10), true},
		{"stale", strconv.FormatInt(now.Add(-ecuMotionMaxAge-time.Millisecond).UnixMilli(), 10), false},
		{"future", strconv.FormatInt(now.Add(time.Millisecond).UnixMilli(), 10), false},
		{"missing", nil, false},
		{"invalid", "invalid", false},
		{"zero", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ecuMotionFresh(tc.value, now); got != tc.want {
				t.Fatalf("ecuMotionFresh(%v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
