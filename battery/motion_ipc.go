package battery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const motionEvidenceChannel = "engine-ecu:motion"

// Required fields use pointers so omitted zero/false values cannot establish
// a stop. This is decoded ECU evidence, with no CAN framing in this service.
type motionEvidence struct {
	Version    int        `json:"version"`
	Source     string     `json:"source"`
	Sequence   uint64     `json:"sequence"`
	ObservedAt *time.Time `json:"observed_at"`
	Valid      *bool      `json:"valid"`
	RPM        *uint16    `json:"rpm"`
	RawSpeed   *uint8     `json:"raw_speed"`
	ThrottleOn *bool      `json:"throttle_on"`
	Reason     string     `json:"reason"`
}

func (a *packAdmission) resetMotionStream() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invalidateMotionLocked()
	a.motion.source, a.motion.sequence = "", 0
}

func (a *packAdmission) receiveMotionEvidence(payload string, now time.Time) bool {
	var e motionEvidence
	valid := len(payload) <= 1024 && json.Unmarshal([]byte(payload), &e) == nil
	source, err := hex.DecodeString(e.Source)
	valid = valid && err == nil && len(source) == 16 && e.Source == strings.ToLower(e.Source) && e.Version == 1 && e.Sequence != 0 && e.Valid != nil
	a.mu.Lock()
	defer a.mu.Unlock()
	m := &a.motion
	if motionClockJump(m.lastCheck, now) {
		a.invalidateMotionLocked()
	}
	m.lastCheck = now
	if !valid {
		a.invalidateMotionLocked()
		return false
	}
	if e.Source == m.source && e.Sequence <= m.sequence {
		a.invalidateMotionLocked()
		return false
	}
	// Sequence gaps include dropped unsafe reports, not just harmless lost
	// refreshes. Reconnects and publisher restarts likewise need a full window.
	if e.Source != m.source || e.Sequence != m.sequence+1 {
		a.invalidateMotionLocked()
	}
	m.source, m.sequence = e.Source, e.Sequence
	if !*e.Valid || e.ObservedAt == nil || e.RPM == nil || e.RawSpeed == nil || e.ThrottleOn == nil {
		a.invalidateMotionLocked()
		return false
	}
	age := now.Sub(*e.ObservedAt)
	if e.ObservedAt.IsZero() || age < 0 || age > stoppedMotionMaxAge {
		a.invalidateMotionLocked()
		return false
	}
	received := now.Add(-age)
	a.observeMotionLocked(*e.RPM == 0 && *e.RawSpeed == 0 && !*e.ThrottleOn, received, now)
	return !m.at.IsZero()
}

func (s *Service) receiveMotionMessages(pubsub *redis.PubSub) error {
	reported := false
	for {
		message, err := pubsub.Receive(s.ctx)
		if err != nil {
			return err
		}
		switch msg := message.(type) {
		case *redis.Subscription:
			s.admission.resetMotionStream()
			reported = false
		case *redis.Message:
			if msg.Channel == motionEvidenceChannel && s.admission.receiveMotionEvidence(msg.Payload, time.Now()) && !reported {
				s.logger.Info("Received fresh motion evidence from ecu-service")
				reported = true
			}
		}
	}
}

func (s *Service) runMotionSubscriber() {
	defer s.motionWG.Done()
	defer s.admission.resetMotionStream()
	for s.ctx.Err() == nil {
		pubsub := s.redis.Subscribe(s.ctx, motionEvidenceChannel)
		closeOnCancel := context.AfterFunc(s.ctx, func() { _ = pubsub.Close() })
		err := s.receiveMotionMessages(pubsub)
		closeOnCancel()
		_ = pubsub.Close()
		s.admission.resetMotionStream()
		if s.ctx.Err() != nil {
			return
		}
		s.logger.Warn(fmt.Sprintf("Motion evidence subscription unavailable: %v; retrying in 5s", err))
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
