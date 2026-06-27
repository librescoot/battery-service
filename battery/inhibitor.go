package battery

import (
	"fmt"
	"net"
	"time"
)

// suspendInhibitorSocket is pm-service's inhibitor coordination socket.
// pm-service registers one block inhibitor per live connection and will not
// suspend or hibernate while any is held; closing the connection (or this
// process dying) releases it. It is a var (not a const) so tests can point it
// at a mock listener.
var suspendInhibitorSocket = "/tmp/suspend_inhibitor"

// inhibitorDialTimeout caps the connect + ack wait. pm-service acks
// immediately on accept; if it is slow or absent we must not block NFC
// operations indefinitely.
const inhibitorDialTimeout = 3 * time.Second

// SuspendInhibitor prevents system suspend during critical NFC operations by
// holding a connection to pm-service's /tmp/suspend_inhibitor socket open. The
// inhibitor is released when the connection is closed.
//
// pm-service gates suspend on its own registry (this socket + the power:inhibits
// hash); holding a connection here registers a block inhibitor for the lifetime
// of the critical section.
type SuspendInhibitor struct {
	conn net.Conn
	name string
}

// NewSuspendInhibitor opens and holds a pm-service suspend inhibitor.
//
//   - name: application identifier (e.g. "BATTERY_NFC_TRANSACTION_INHIBITOR"), for logging
//   - why:  reason for inhibiting (e.g. "NFC_TRANSACTION"), for logging
//   - mode: "block" (hold the connection until released) or "delay" (released
//     immediately; the socket only offers block semantics, so delay does not
//     hold anything)
//
// The connection is held open for the lifetime of the inhibitor. pm-service
// drops the inhibitor automatically if this process dies, so a crash mid-write
// can never wedge power management.
func NewSuspendInhibitor(name, why, mode string) (*SuspendInhibitor, error) {
	conn, err := net.DialTimeout("unix", suspendInhibitorSocket, inhibitorDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to suspend inhibitor socket: %w", err)
	}

	// pm-service sends a one-byte ack on accept. Wait for it (bounded) so we
	// know the inhibitor is registered before returning.
	if err := conn.SetReadDeadline(time.Now().Add(inhibitorDialTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to set inhibitor read deadline: %w", err)
	}
	ack := make([]byte, 1)
	if _, err := conn.Read(ack); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read inhibitor ack: %w", err)
	}
	// Clear the deadline: the connection is now held indefinitely. We never read
	// again — closing it is what releases the inhibitor.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to clear inhibitor read deadline: %w", err)
	}

	si := &SuspendInhibitor{conn: conn, name: name}

	// A "delay" inhibitor is non-blocking at the socket layer: release the
	// connection right away.
	if mode == "delay" {
		conn.Close()
		si.conn = nil
	}

	return si, nil
}

// Release closes the inhibitor connection, releasing the lock and allowing the
// system to suspend again.
func (si *SuspendInhibitor) Release() error {
	if si.conn == nil {
		return nil // already released (or delay mode)
	}
	err := si.conn.Close()
	si.conn = nil
	if err != nil {
		return fmt.Errorf("failed to close inhibitor connection: %w", err)
	}
	return nil
}

// IsActive returns true if the inhibitor connection is still held.
func (si *SuspendInhibitor) IsActive() bool {
	return si.conn != nil
}
