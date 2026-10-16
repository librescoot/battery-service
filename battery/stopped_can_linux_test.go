package battery

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStoppedStatusFilterSelectsStandardDataFrames(t *testing.T) {
	if stoppedStatusCANMask&unix.CAN_ERR_FLAG != 0 {
		t.Fatal("data filter would be registered as an error filter")
	}
	if stoppedStatusCANMask != unix.CAN_SFF_MASK|unix.CAN_EFF_FLAG|unix.CAN_RTR_FLAG {
		t.Fatal("filter must match the full standard ID and exclude extended and remote frames")
	}
	for _, id := range []uint32{stoppedStatusCANID, 0x7e1, stoppedStatusCANID | unix.CAN_EFF_FLAG, stoppedStatusCANID | unix.CAN_RTR_FLAG} {
		if matches := id&stoppedStatusCANMask == stoppedStatusCANID; matches != (id == stoppedStatusCANID) {
			t.Fatalf("CAN ID %#x matches=%t", id, matches)
		}
	}
}

func TestStoppedStatusRequiresZeroRPMZeroSpeedAndThrottleOff(t *testing.T) {
	for _, tc := range []struct {
		name           string
		id             uint32
		length         byte
		rpm            uint16
		speed, flags   byte
		stopped, valid bool
	}{
		{"stopped", stoppedStatusCANID, 8, 0, 0, 0, true, true},
		{"brake applied", stoppedStatusCANID, 8, 0, 0, 2, true, true},
		{"throttle", stoppedStatusCANID, 8, 0, 0, 1, false, true},
		{"sub-kmh movement", stoppedStatusCANID, 8, 1, 0, 0, false, true},
		{"raw speed", stoppedStatusCANID, 8, 0, 1, 0, false, true},
		{"invalid RPM", stoppedStatusCANID, 8, 65535, 0, 0, false, true},
		{"short payload", stoppedStatusCANID, 7, 0, 0, 0, false, false},
		{"other frame", 0x7e1, 8, 0, 0, 0, false, false},
		{"remote request", stoppedStatusCANID | unix.CAN_RTR_FLAG, 8, 0, 0, 0, false, false},
		{"extended frame", stoppedStatusCANID | unix.CAN_EFF_FLAG, 8, 0, 0, 0, false, false},
		{"CAN error", stoppedStatusCANID | unix.CAN_ERR_FLAG, 8, 0, 0, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var frame [16]byte
			binary.NativeEndian.PutUint32(frame[:4], tc.id)
			frame[4] = tc.length
			binary.BigEndian.PutUint16(frame[12:14], tc.rpm)
			frame[14], frame[15] = tc.speed, tc.flags
			stopped, valid := stoppedStatusFrame(frame[:])
			if stopped != tc.stopped || valid != tc.valid {
				t.Fatalf("stopped=%t valid=%t", stopped, valid)
			}
		})
	}
	for _, n := range []int{0, 8, 15, 17} {
		if _, valid := stoppedStatusFrame(make([]byte, n)); valid {
			t.Fatalf("accepted %d-byte frame", n)
		}
	}
}

func TestStoppedKernelReceiveTimestamp(t *testing.T) {
	now := admissionTestTime.Add(750 * time.Millisecond)
	for _, size := range []int{8, 16} {
		for _, age := range []time.Duration{0, stoppedMotionMaxAge, stoppedMotionMaxAge + time.Nanosecond, -time.Nanosecond} {
			at := now.Add(-age)
			data := make([]byte, size)
			if size == 8 {
				binary.NativeEndian.PutUint32(data[:4], uint32(at.Unix()))
				binary.NativeEndian.PutUint32(data[4:], uint32(at.Nanosecond()))
			} else {
				binary.NativeEndian.PutUint64(data[:8], uint64(at.Unix()))
				binary.NativeEndian.PutUint64(data[8:], uint64(at.Nanosecond()))
			}
			received, err := stoppedReceiveTime(data, now)
			want := age >= 0 && age <= stoppedMotionMaxAge
			if (err == nil) != want || (want && !received.Equal(at)) {
				t.Fatalf("size=%d age=%s received=%v error=%v", size, age, received, err)
			}
		}
	}
	for _, data := range [][]byte{nil, make([]byte, 7), make([]byte, 8), make([]byte, 16), make([]byte, 24)} {
		if _, err := stoppedReceiveTime(data, now); err == nil {
			t.Fatal("accepted malformed timestamp")
		}
	}
	invalid := make([]byte, 16)
	binary.NativeEndian.PutUint64(invalid[:8], uint64(now.Unix()))
	binary.NativeEndian.PutUint64(invalid[8:], uint64(time.Second))
	if _, err := stoppedReceiveTime(invalid, now); err == nil {
		t.Fatal("accepted invalid nanoseconds")
	}
}

func TestStoppedCANMetadataPreservesReceiveFreshness(t *testing.T) {
	for _, scenario := range []string{"fresh", "missing timestamp", "queued frame", "overflow", "unchanged overflow counter", "malformed overflow"} {
		t.Run(scenario, func(t *testing.T) {
			a, now := readyStoppedPolicy(t)
			now = now.Add(100 * time.Millisecond)
			stamp := now
			if scenario == "queued frame" {
				stamp = now.Add(-stoppedMotionMaxAge - time.Nanosecond)
			}
			data := make([]byte, 16)
			binary.NativeEndian.PutUint64(data[:8], uint64(stamp.Unix()))
			binary.NativeEndian.PutUint64(data[8:], uint64(stamp.Nanosecond()))
			messages := []unix.SocketControlMessage{{Header: unix.Cmsghdr{Level: unix.SOL_SOCKET, Type: unix.SCM_TIMESTAMPNS}, Data: data}}
			var lastDrops uint32
			switch scenario {
			case "missing timestamp":
				messages = nil
			case "overflow", "unchanged overflow counter", "malformed overflow":
				drops := make([]byte, 4)
				binary.NativeEndian.PutUint32(drops, 1)
				if scenario == "unchanged overflow counter" {
					lastDrops = 1
				}
				if scenario == "malformed overflow" {
					drops = drops[:3]
				}
				messages = append(messages, unix.SocketControlMessage{Header: unix.Cmsghdr{Level: unix.SOL_SOCKET, Type: unix.SO_RXQ_OVFL}, Data: drops})
			}
			a.observeCANMotion(true, messages, now, lastDrops)
			requireAdmission(t, a, 1, now, scenario == "fresh" || scenario == "unchanged overflow counter")
			requireAdmission(t, a, 0, now, true)
		})
	}
}

func TestStoppedCANMonitorHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Service{ctx: ctx, config: &ServiceConfig{}, admission: samplingPolicy()}
	if err := s.readStoppedMotion(ctx, -1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
	s.admission.motion.enabled = true
	s.admission.observeMotion(true, admissionTestTime, admissionTestTime)
	s.motionWG.Add(1)
	s.runStoppedAdmissionMonitor()
	s.motionWG.Wait()
	if s.admission.motion.stopped {
		t.Fatal("shutdown retained motion evidence")
	}
}
