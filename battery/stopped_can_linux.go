package battery

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

const stoppedStatusCANID = 0x7e0

func openStoppedMotionSocket(device string) (int, error) {
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Socket(unix.AF_CAN, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.CAN_RAW)
	if err != nil {
		return -1, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(fd)
		}
	}()
	filter := []unix.CanFilter{{Id: stoppedStatusCANID, Mask: unix.CAN_SFF_MASK | unix.CAN_EFF_FLAG | unix.CAN_RTR_FLAG | unix.CAN_ERR_FLAG}}
	if err = unix.SetsockoptCanRawFilter(fd, unix.SOL_CAN_RAW, unix.CAN_RAW_FILTER, filter); err != nil {
		return -1, err
	}
	for _, opt := range []struct{ level, name, value int }{
		{unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1},
		{unix.SOL_SOCKET, unix.SO_RXQ_OVFL, 1},
		{unix.SOL_CAN_RAW, unix.CAN_RAW_ERR_FILTER, unix.CAN_ERR_MASK},
	} {
		if err = unix.SetsockoptInt(fd, opt.level, opt.name, opt.value); err != nil {
			return -1, err
		}
	}
	if err = unix.Bind(fd, &unix.SockaddrCAN{Ifindex: iface.Index}); err != nil {
		return -1, err
	}
	ok = true
	return fd, nil
}

// Status1 layout is also decoded by ecu-service/bosch.go. Requiring RPM zero
// avoids treating speed truncated to whole km/h as proof of a stop.
func stoppedStatusFrame(frame []byte) (stopped, valid bool) {
	if len(frame) != 16 || binary.NativeEndian.Uint32(frame[:4]) != stoppedStatusCANID || frame[4] != 8 {
		return false, false
	}
	return binary.BigEndian.Uint16(frame[12:14]) == 0 && frame[14] == 0 && frame[15]&1 == 0, true
}

// SO_TIMESTAMPNS uses native long widths: ARMv7 timespec is 8 bytes, amd64 16.
// Convert kernel wall time to the local monotonic timeline only after bounding
// its age. Dequeue time must never make a buffered CAN frame look fresh.
func stoppedReceiveTime(data []byte, now time.Time) (time.Time, error) {
	var sec, nsec int64
	switch len(data) {
	case 8:
		sec = int64(int32(binary.NativeEndian.Uint32(data[:4])))
		nsec = int64(int32(binary.NativeEndian.Uint32(data[4:])))
	case 16:
		sec = int64(binary.NativeEndian.Uint64(data[:8]))
		nsec = int64(binary.NativeEndian.Uint64(data[8:]))
	default:
		return time.Time{}, fmt.Errorf("invalid CAN timestamp size %d", len(data))
	}
	if sec <= 0 || nsec < 0 || nsec >= int64(time.Second) {
		return time.Time{}, fmt.Errorf("invalid CAN receive timestamp")
	}
	age := now.Sub(time.Unix(sec, nsec))
	if age < 0 || age > stoppedMotionMaxAge {
		return time.Time{}, fmt.Errorf("CAN receive timestamp outside freshness window")
	}
	return now.Add(-age), nil
}

func (s *Service) readStoppedMotion(ctx context.Context, fd int) error {
	defer s.admission.invalidateMotion()
	var frame [16]byte
	var control [128]byte
	var lastDrops uint32
	for ctx.Err() == nil {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(poll, 50); err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return fmt.Errorf("CAN socket unavailable: poll=%#x", poll[0].Revents)
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, oobn, flags, _, err := unix.Recvmsg(fd, frame[:], control[:], unix.MSG_DONTWAIT)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		now := time.Now()
		stopped, valid := stoppedStatusFrame(frame[:n])
		if !valid || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			s.admission.invalidateMotion()
			continue
		}
		messages, err := unix.ParseSocketControlMessage(control[:oobn])
		if err != nil {
			s.admission.invalidateMotion()
			continue
		}
		lastDrops = s.admission.observeCANMotion(stopped, messages, now, lastDrops)
	}
	return ctx.Err()
}

func (a *packAdmission) observeCANMotion(stopped bool, messages []unix.SocketControlMessage, now time.Time, lastDrops uint32) uint32 {
	var received time.Time
	for _, msg := range messages {
		if msg.Header.Level != unix.SOL_SOCKET {
			continue
		}
		switch msg.Header.Type {
		case unix.SCM_TIMESTAMPNS:
			var err error
			received, err = stoppedReceiveTime(msg.Data, now)
			if err != nil {
				a.invalidateMotion()
				return lastDrops
			}
		case unix.SO_RXQ_OVFL:
			if len(msg.Data) != 4 {
				a.invalidateMotion()
				return lastDrops
			}
			drops := binary.NativeEndian.Uint32(msg.Data)
			if drops != lastDrops {
				a.invalidateMotion()
				lastDrops = drops
			}
		}
	}
	a.observeMotion(stopped, received, now)
	return lastDrops
}

func (s *Service) runStoppedAdmissionMonitor() {
	defer s.motionWG.Done()
	defer s.admission.invalidateMotion()
	device := s.config.AdmissionCANDevice
	if device == "" {
		device = "can0"
	}
	for s.ctx.Err() == nil {
		fd, err := openStoppedMotionSocket(device)
		if err == nil {
			s.logger.Info(fmt.Sprintf("Stopped-vehicle admission monitoring CAN Status1 on %s (receive only)", device))
			err = s.readStoppedMotion(s.ctx, fd)
			_ = unix.Close(fd)
		}
		s.admission.invalidateMotion()
		if s.ctx.Err() != nil {
			return
		}
		s.logger.Warn(fmt.Sprintf("Stopped-vehicle admission unavailable: %v; retrying in 5s", err))
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
