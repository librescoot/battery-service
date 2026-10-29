package battery

import (
	"time"

	"battery-service/battery/fsm"
	"github.com/redis/go-redis/v9"
)

const admissionDemandTimeout = 30 * time.Second

func (s *Service) handleDriveStartStatusMessage() {
	status, err := s.redis.HGet(s.ctx, "vehicle", "drive-start-status").Result()
	if err != nil && err != redis.Nil {
		s.logger.Warn("Failed to read drive-start status", "error", err)
		return
	}
	s.setDriveStartStatus(status)
}

func (s *Service) setDriveStartStatus(status string) {
	if status != "waiting-battery" {
		s.driveStartDeadline.Store(nil)
		return
	}
	deadline := time.Now().Add(admissionDemandTimeout)
	if s.driveStartDeadline.CompareAndSwap(nil, &deadline) {
		select {
		case s.admissionRequests <- struct{}{}:
		default:
		}
	}
}

func (s *Service) driveStartDemandActive() bool {
	deadline := s.driveStartDeadline.Load()
	return deadline != nil && time.Now().Before(*deadline)
}

func (s *Service) notifyAdmissionReady() {
	if !s.driveStartDemandActive() {
		return
	}
	select {
	case s.admissionReady <- struct{}{}:
	default:
	}
}

func (s *Service) pollAdmission(demanded bool) {
	if s.admission == nil {
		return
	}
	now := time.Now()
	s.logAdmissionDiagnostics(now)
	id, report := s.admission.beginRound(now)
	s.logSamplingReport(report)
	// An existing round already requests both readers. Do not overlap it or
	// replace its samples. Single-reader configurations still support demand.
	if id != 0 || (demanded && !s.admission.roundInProgress()) {
		s.promptAdmissionReaders()
	}
}

func (s *Service) promptAdmissionReaders() {
	for _, reader := range s.readers {
		if reader != nil && reader.fsm != nil {
			reader.fsm.SendEvent(fsm.EvAdmissionPoll)
		}
	}
}

func (a *packAdmission) roundInProgress() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sampling.round != nil
}

func (a *packAdmission) hasInactiveCandidateLocked() bool {
	if a.sampling.stopped {
		return false
	}
	for _, s := range a.slots {
		if s.eligible && !s.absent && !s.held && !s.offPending && !s.active {
			return true
		}
	}
	return false
}
