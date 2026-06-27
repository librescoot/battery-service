package battery

import (
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakePMSocket mimics pm-service's /tmp/suspend_inhibitor server: on each
// accepted connection it writes a one-byte ack, then reads until the client
// closes (which is how pm-service detects an inhibitor release). It records how
// many connections are currently held.
type fakePMSocket struct {
	ln   net.Listener
	mu   sync.Mutex
	held int
}

func newFakePMSocket(t *testing.T) *fakePMSocket {
	t.Helper()
	path := filepath.Join(t.TempDir(), "suspend_inhibitor")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakePMSocket{ln: ln}

	old := suspendInhibitorSocket
	suspendInhibitorSocket = path
	t.Cleanup(func() {
		suspendInhibitorSocket = old
		ln.Close()
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.held++
			f.mu.Unlock()
			go func(c net.Conn) {
				c.Write([]byte{0}) // ack
				buf := make([]byte, 1)
				for {
					if _, err := c.Read(buf); err != nil {
						break // client closed -> release
					}
				}
				f.mu.Lock()
				f.held--
				f.mu.Unlock()
				c.Close()
			}(conn)
		}
	}()

	return f
}

func (f *fakePMSocket) heldCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

func waitFor(t *testing.T, want int, get func() int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met: got %d, want %d", get(), want)
}

// A block inhibitor must hold the socket connection open until released, so
// pm-service sees a live blocker for the whole NFC critical section.
func TestBlockInhibitorHoldsConnection(t *testing.T) {
	pm := newFakePMSocket(t)

	inh, err := NewSuspendInhibitor("BATTERY_NFC_TRANSACTION_INHIBITOR", "NFC_TRANSACTION", "block")
	if err != nil {
		t.Fatalf("NewSuspendInhibitor: %v", err)
	}
	if !inh.IsActive() {
		t.Fatal("block inhibitor should be active after acquire")
	}
	waitFor(t, 1, pm.heldCount) // pm-service sees the block

	if err := inh.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if inh.IsActive() {
		t.Fatal("inhibitor should be inactive after release")
	}
	waitFor(t, 0, pm.heldCount) // closing the connection releases it
}

// Releasing twice must be safe (the FSM can call releaseInhibitor redundantly).
func TestReleaseIsIdempotent(t *testing.T) {
	newFakePMSocket(t)
	inh, err := NewSuspendInhibitor("x", "y", "block")
	if err != nil {
		t.Fatalf("NewSuspendInhibitor: %v", err)
	}
	if err := inh.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := inh.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

// A delay inhibitor is non-blocking at the socket layer: it must not hold a
// connection open.
func TestDelayInhibitorDoesNotHold(t *testing.T) {
	pm := newFakePMSocket(t)
	inh, err := NewSuspendInhibitor("x", "y", "delay")
	if err != nil {
		t.Fatalf("NewSuspendInhibitor: %v", err)
	}
	if inh.IsActive() {
		t.Fatal("delay inhibitor should not be active")
	}
	waitFor(t, 0, pm.heldCount)
}

// With no socket present (pm-service not running) acquire must fail rather than
// block, so the NFC path can fall through to its "without suspend protection"
// warning instead of hanging.
func TestAcquireFailsWithoutServer(t *testing.T) {
	old := suspendInhibitorSocket
	suspendInhibitorSocket = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { suspendInhibitorSocket = old })

	if _, err := NewSuspendInhibitor("x", "y", "block"); err == nil {
		t.Fatal("expected error when socket is absent")
	}
}
