package battery

import (
	"fmt"
	"syscall"

	"github.com/godbus/dbus/v5"
)

// SuspendInhibitor holds a systemd inhibitor lock to prevent system suspend
// during critical NFC operations. The inhibitor is released when the file
// descriptor is closed.
type SuspendInhibitor struct {
	fd   int
	conn *dbus.Conn
	name string
}

// NewSuspendInhibitor acquires a systemd inhibitor lock via D-Bus.
// The inhibitor prevents system suspend and shutdown until released.
//
// - name: Application identifier (e.g., "BATTERY_NFC_TRANSACTION_INHIBITOR")
// - why: Reason for inhibiting (e.g., "NFC_TRANSACTION")
// - mode: "block" (prevent suspend) or "delay" (delay suspend briefly)
//
// Each inhibitor owns a private D-Bus connection so Release() can close it
// without affecting other callers. dbus.SystemBus() returns a process-wide
// shared connection, and closing that would break concurrent users.
func NewSuspendInhibitor(name, why, mode string) (*SuspendInhibitor, error) {
	conn, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to system bus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to authenticate on system bus: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send Hello on system bus: %w", err)
	}

	obj := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	call := obj.Call("org.freedesktop.login1.Manager.Inhibit", 0,
		"sleep:shutdown", // what to inhibit
		name,             // who
		why,              // why
		mode)             // mode

	if call.Err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to acquire inhibitor lock: %w", call.Err)
	}

	// Extract the file descriptor from the response
	var fd dbus.UnixFD
	if err := call.Store(&fd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to extract file descriptor: %w", err)
	}

	return &SuspendInhibitor{
		fd:   int(fd),
		conn: conn,
		name: name,
	}, nil
}

// Release closes the inhibitor file descriptor and DBus connection,
// releasing the lock and allowing the system to suspend again.
func (si *SuspendInhibitor) Release() error {
	if si.fd < 0 {
		return nil // already released
	}

	if err := syscall.Close(si.fd); err != nil {
		if si.conn != nil {
			si.conn.Close()
		}
		return fmt.Errorf("failed to close inhibitor fd: %w", err)
	}

	if si.conn != nil {
		si.conn.Close()
		si.conn = nil
	}

	si.fd = -1
	return nil
}

// IsActive returns true if the inhibitor lock is still held
func (si *SuspendInhibitor) IsActive() bool {
	return si.fd >= 0
}
