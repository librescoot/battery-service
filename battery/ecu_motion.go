package battery

import (
	"context"
	"strconv"
	"time"
)

const (
	ecuMotionReadTimeout = 250 * time.Millisecond
	ecuMotionMaxAge      = 2 * time.Second
)

// sampleECUMotion records whether the ECU's published state shows movement or
// throttle. It reads the engine-ecu hash once, when a decision needs it. The
// hash keeps its last values while the ECU is unpowered, so it only counts
// while the vehicle hash says the engine supply is on.
func (s *Service) sampleECUMotion(now time.Time) {
	a := s.admission
	if a == nil || s.redis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, ecuMotionReadTimeout)
	defer cancel()
	power, err := s.redis.HGet(ctx, "vehicle", "engine-power").Result()
	if err != nil || power != "on" {
		a.invalidateMotion()
		return
	}
	values, err := s.redis.HMGet(ctx, "engine-ecu", "rpm", "raw-speed", "throttle", "updated-at").Result()
	if err != nil || len(values) != 4 || !ecuMotionFresh(values[3], now) {
		a.invalidateMotion()
		return
	}
	stopped, ok := ecuStopped(values)
	if !ok {
		a.invalidateMotion()
		return
	}
	a.observeMotion(stopped, now, now)
}

func ecuMotionFresh(value any, now time.Time) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	ms, err := strconv.ParseInt(text, 10, 64)
	if err != nil || ms <= 0 {
		return false
	}
	age := now.Sub(time.UnixMilli(ms))
	return age >= 0 && age <= ecuMotionMaxAge
}

func ecuStopped(values []any) (stopped, ok bool) {
	rpm, ok0 := values[0].(string)
	speed, ok1 := values[1].(string)
	throttle, ok2 := values[2].(string)
	if !ok0 || !ok1 || !ok2 || (throttle != "on" && throttle != "off") {
		return false, false
	}
	r, err0 := strconv.ParseUint(rpm, 10, 16)
	v, err1 := strconv.ParseUint(speed, 10, 8)
	if err0 != nil || err1 != nil {
		return false, false
	}
	return r == 0 && v == 0 && throttle == "off", true
}
