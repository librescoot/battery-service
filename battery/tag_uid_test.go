package battery

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// newUIDTestReader builds the smallest reader that noteTagPresent needs: a
// logger it can write to and the UID field it tracks.
func newUIDTestReader(buf *bytes.Buffer) *BatteryReader {
	return &BatteryReader{
		index:  0,
		logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func TestNoteTagPresentReportsEachUIDOnce(t *testing.T) {
	var buf bytes.Buffer
	r := newUIDTestReader(&buf)

	uidA := []byte{0x04, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x01}
	uidB := []byte{0x04, 0x11, 0x22, 0x33, 0x44, 0x55, 0x02}

	r.noteTagPresent(uidA)
	if got := strings.Count(buf.String(), "Tag arrived: UID=04AABBCCDDEE01"); got != 1 {
		t.Fatalf("first sighting of a UID logged %d times, want 1:\n%s", got, buf.String())
	}

	// Discovery runs again after every command cycle. The same UID must not be
	// reported again, or the log fills with one line every few seconds.
	buf.Reset()
	r.noteTagPresent(uidA)
	r.noteTagPresent(uidA)
	if strings.Contains(buf.String(), "Tag arrived") {
		t.Errorf("repeat sighting of the same UID logged an arrival:\n%s", buf.String())
	}

	// A different tag is a different side of a pack, or a different pack.
	buf.Reset()
	r.noteTagPresent(uidB)
	if !strings.Contains(buf.String(), "Tag arrived: UID=04112233445502") {
		t.Errorf("a changed UID was not reported:\n%s", buf.String())
	}
	if !bytes.Equal(r.currentTagUID, uidB) {
		t.Errorf("currentTagUID = %X, want %X", r.currentTagUID, uidB)
	}
}

// The reader must not alias the HAL's slice, which the next DetectTags call is
// free to reuse: an aliased UID would silently compare equal forever.
func TestNoteTagPresentCopiesUID(t *testing.T) {
	var buf bytes.Buffer
	r := newUIDTestReader(&buf)

	uid := []byte{0x04, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x01}
	r.noteTagPresent(uid)
	uid[1] = 0x99

	if r.currentTagUID[1] != 0xAA {
		t.Fatal("currentTagUID aliases the caller's slice")
	}

	buf.Reset()
	r.noteTagPresent(uid)
	if !strings.Contains(buf.String(), "Tag arrived") {
		t.Errorf("a UID that changed under the reader was not reported:\n%s", buf.String())
	}
}

func TestNoteTagPresentIgnoresEmptyUID(t *testing.T) {
	var buf bytes.Buffer
	r := newUIDTestReader(&buf)

	r.noteTagPresent(nil)
	if strings.Contains(buf.String(), "Tag arrived") {
		t.Errorf("an empty UID was reported as an arrival:\n%s", buf.String())
	}
	if r.currentTagUID != nil {
		t.Errorf("currentTagUID = %X, want nil", r.currentTagUID)
	}
}
