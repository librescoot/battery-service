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
	name string
}

// NewSuspendInhibitor acquires a systemd inhibitor lock via D-Bus.
// The inhibitor prevents system suspend and shutdown until released.
//
// - name: Application identifier (e.g., "BATTERY_NFC_TRANSACTION_INHIBITOR")
// - why: Reason for inhibiting (e.g., "NFC_TRANSACTION")
// - mode: "block" (prevent suspend) or "delay" (delay suspend briefly)
//
// Returns nil if the inhibitor cannot be acquired (e.g., D-Bus unavailable,
// suspend already in progress).
func NewSuspendInhibitor(name, why, mode string) (*SuspendInhibitor, error) {
	// Connect to system bus
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to system bus: %w", err)
	}
	defer conn.Close()

	// Call the Inhibit method on systemd's login manager
	obj := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	call := obj.Call("org.freedesktop.login1.Manager.Inhibit", 0,
		"sleep:shutdown", // what to inhibit
		name,             // who
		why,              // why
		mode)             // mode

	if call.Err != nil {
		return nil, fmt.Errorf("failed to acquire inhibitor lock: %w", call.Err)
	}

	// Extract the file descriptor from the response
	var fd dbus.UnixFD
	if err := call.Store(&fd); err != nil {
		return nil, fmt.Errorf("failed to extract file descriptor: %w", err)
	}

	return &SuspendInhibitor{
		fd:   int(fd),
		name: name,
	}, nil
}

// Release closes the inhibitor file descriptor, releasing the lock
// and allowing the system to suspend again.
func (si *SuspendInhibitor) Release() error {
	if si.fd < 0 {
		return nil // already released
	}

	if err := syscall.Close(si.fd); err != nil {
		return fmt.Errorf("failed to close inhibitor fd: %w", err)
	}

	si.fd = -1
	return nil
}

// IsActive returns true if the inhibitor lock is still held
func (si *SuspendInhibitor) IsActive() bool {
	return si.fd >= 0
}
