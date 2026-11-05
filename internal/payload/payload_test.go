package payload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"
)

// readAll drains a NewReader into a byte slice.
func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading payload: %v", err)
	}
	return b
}

// TestReaderIsDeterministic asserts the same seed and size yield byte-identical
// data, the property that makes a scenario push an identical synthetic payload
// every run.
func TestReaderIsDeterministic(t *testing.T) {
	a := readAll(t, NewReader(7, 4096))
	b := readAll(t, NewReader(7, 4096))
	if !bytes.Equal(a, b) {
		t.Error("NewReader with the same seed produced different bytes")
	}
}

// TestReaderVariesWithSeed asserts a different seed yields different data, so
// distinct resources push distinct payloads.
func TestReaderVariesWithSeed(t *testing.T) {
	a := readAll(t, NewReader(7, 4096))
	b := readAll(t, NewReader(8, 4096))
	if bytes.Equal(a, b) {
		t.Error("NewReader produced identical bytes for different seeds")
	}
}

// TestReaderExactLength asserts the reader yields exactly the requested number
// of bytes, so the byte volume pushed is the scenario parameter and not an
// accident of the RNG.
func TestReaderExactLength(t *testing.T) {
	const n = 3 * 1024 * 1024
	if got := len(readAll(t, NewReader(1, n))); got != n {
		t.Errorf("payload length = %d, want %d", got, n)
	}
}

// TestReaderRewindsToStart asserts a rewind after a partial read and its Close
// yields the whole payload again, which a repeated upload sends.
func TestReaderRewindsToStart(t *testing.T) {
	r := NewReader(7, 4096)
	if _, err := io.ReadFull(r, make([]byte, 1000)); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pos, err := r.Seek(0, io.SeekStart); pos != 0 || err != nil {
		t.Fatalf("Seek(0, io.SeekStart) = %d, %v, want 0, nil", pos, err)
	}
	got := readAll(t, r)
	if len(got) != 4096 {
		t.Errorf("payload length after rewind = %d, want 4096", len(got))
	}
	if !bytes.Equal(got, readAll(t, NewReader(7, 4096))) {
		t.Error("payload after rewind differs from a fresh payload")
	}
}

// TestReaderRewindWaitsForClose asserts a rewind waits until the current attempt
// is closed, since net/http may still read the previous upload after RoundTrip
// returned its 401 and a rewind must not hand it the new stream.
func TestReaderRewindWaitsForClose(t *testing.T) {
	r := NewReader(7, 4096)
	rewound := make(chan error, 1)
	go func() {
		_, err := r.Seek(0, io.SeekStart)
		rewound <- err
	}()
	select {
	case err := <-rewound:
		t.Fatalf("Seek returned %v before the attempt was closed", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-rewound; err != nil {
		t.Errorf("Seek after Close = %v, want nil", err)
	}
}

// TestReaderRejectsOtherSeeks asserts every seek but a rewind to the start
// fails, since a partial rewind of a random stream has no meaning.
func TestReaderRejectsOtherSeeks(t *testing.T) {
	tests := []struct {
		name   string
		offset int64
		whence int
	}{
		{"forward from start", 1, io.SeekStart},
		{"from current", 0, io.SeekCurrent},
		{"from end", 0, io.SeekEnd},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReader(7, 4096).Seek(tc.offset, tc.whence); err == nil {
				t.Errorf("Seek(%d, %d) succeeded, want an error", tc.offset, tc.whence)
			}
		})
	}
}

// TestReaderZeroLength asserts an empty payload reads as empty and still
// rewinds.
func TestReaderZeroLength(t *testing.T) {
	r := NewReader(7, 0)
	b, err := io.ReadAll(r)
	if err != nil || len(b) != 0 {
		t.Errorf("reading an empty payload = %d bytes, %v, want 0 bytes, nil", len(b), err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if pos, err := r.Seek(0, io.SeekStart); pos != 0 || err != nil {
		t.Errorf("Seek(0, io.SeekStart) = %d, %v, want 0, nil", pos, err)
	}
}

// TestSeedVariesPerLogicalName asserts two logical names under one plan seed
// derive distinct payload seeds, so two resources never share a payload, and
// that the derived seed varies with the plan seed.
func TestSeedVariesPerLogicalName(t *testing.T) {
	if Seed(42, "img-0001") == Seed(42, "img-0002") {
		t.Error("Seed collided for two distinct logical names")
	}
	if Seed(42, "img-0001") == Seed(43, "img-0001") {
		t.Error("Seed did not vary with the plan seed")
	}
}

// TestSeedIsStable asserts the same (plan seed, logical name) pair always
// derives the same seed, so a scenario's uploads are byte-identical run to run.
func TestSeedIsStable(t *testing.T) {
	const seed, name = 42, "img-0001"
	first := Seed(seed, name)
	if second := Seed(seed, name); first != second {
		t.Errorf("Seed unstable: %d != %d", first, second)
	}
}

// The pinned values below were computed from the derivation as Glance shipped
// it before the code moved here. A change to either one changes the bytes every
// existing scenario writes, so a failing pin means the derivation changed, not
// that the constant is stale.
const (
	pinnedSeed      int64 = -4201207012212022334 // Seed(42, "img-0001")
	pinnedReaderSHA       = "1ba2f22d9117762764503f7dec925c9f99c68e68b31b925e68425f6953ac3f33"
)

// TestSeedIsPinned asserts Seed still derives the value it derived before the
// move, so a plan's payload seeds are the same across dizzy versions and not
// only within one process.
func TestSeedIsPinned(t *testing.T) {
	if got := Seed(42, "img-0001"); got != pinnedSeed {
		t.Errorf("Seed(42, %q) = %d, want %d", "img-0001", got, pinnedSeed)
	}
}

// TestReaderIsPinned asserts NewReader still yields the bytes it yielded before
// the move, by the SHA-256 of a 4096-byte payload.
func TestReaderIsPinned(t *testing.T) {
	b := readAll(t, NewReader(7, 4096))
	if len(b) != 4096 {
		t.Fatalf("payload length = %d, want 4096", len(b))
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != pinnedReaderSHA {
		t.Errorf("SHA-256 of NewReader(7, 4096) = %s, want %s", got, pinnedReaderSHA)
	}
}

// TestBytesMatchesReader asserts Bytes returns exactly the bytes NewReader
// yields for the same seed and length, so a payload sent whole matches one
// streamed, and that a zero length yields an empty slice.
func TestBytesMatchesReader(t *testing.T) {
	want := readAll(t, NewReader(7, 4096))
	if got := Bytes(7, 4096); !bytes.Equal(got, want) {
		t.Errorf("Bytes(7, 4096) differs from NewReader(7, 4096): %d bytes, want %d", len(got), len(want))
	}
	if got := Bytes(7, 0); len(got) != 0 {
		t.Errorf("Bytes(7, 0) has length %d, want 0", len(got))
	}
}
