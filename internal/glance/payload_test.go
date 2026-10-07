package glance

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// readAll drains a payloadReader into a byte slice.
func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading payload: %v", err)
	}
	return b
}

// TestPayloadReaderIsDeterministic asserts the same seed and size yield
// byte-identical data, the property that makes a scenario push an identical
// synthetic payload every run.
func TestPayloadReaderIsDeterministic(t *testing.T) {
	a := readAll(t, payloadReader(7, 4096))
	b := readAll(t, payloadReader(7, 4096))
	if !bytes.Equal(a, b) {
		t.Error("payloadReader with the same seed produced different bytes")
	}
}

// TestPayloadReaderVariesWithSeed asserts a different seed yields different data,
// so distinct images push distinct payloads.
func TestPayloadReaderVariesWithSeed(t *testing.T) {
	a := readAll(t, payloadReader(7, 4096))
	b := readAll(t, payloadReader(8, 4096))
	if bytes.Equal(a, b) {
		t.Error("payloadReader produced identical bytes for different seeds")
	}
}

// TestPayloadReaderExactLength asserts the reader yields exactly the requested
// number of bytes, so the byte volume pushed is the scenario parameter and not
// an accident of the RNG.
func TestPayloadReaderExactLength(t *testing.T) {
	const n = 3 * 1024 * 1024
	if got := len(readAll(t, payloadReader(1, n))); got != n {
		t.Errorf("payload length = %d, want %d", got, n)
	}
}

// TestPayloadReaderRewindsToStart asserts a rewind after a partial read and its
// Close yields the whole payload again, which a repeated upload sends.
func TestPayloadReaderRewindsToStart(t *testing.T) {
	r := payloadReader(7, 4096)
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
	if !bytes.Equal(got, readAll(t, payloadReader(7, 4096))) {
		t.Error("payload after rewind differs from a fresh payload")
	}
}

// TestPayloadReaderRewindWaitsForClose asserts a rewind waits until the current
// attempt is closed, since net/http may still read the previous upload after
// RoundTrip returned its 401 and a rewind must not hand it the new stream.
func TestPayloadReaderRewindWaitsForClose(t *testing.T) {
	r := payloadReader(7, 4096)
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

// TestPayloadReaderRejectsOtherSeeks asserts every seek but a rewind to the
// start fails, since a partial rewind of a random stream has no meaning.
func TestPayloadReaderRejectsOtherSeeks(t *testing.T) {
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
			if _, err := payloadReader(7, 4096).Seek(tc.offset, tc.whence); err == nil {
				t.Errorf("Seek(%d, %d) succeeded, want an error", tc.offset, tc.whence)
			}
		})
	}
}

// TestPayloadReaderZeroLength asserts an empty payload reads as empty and still
// rewinds.
func TestPayloadReaderZeroLength(t *testing.T) {
	r := payloadReader(7, 0)
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

// TestPayloadSeedVariesPerLogicalName asserts two logical names under one plan
// seed derive distinct payload seeds, so two images never share a payload, while
// the same (seed, name) pair is stable.
func TestPayloadSeedVariesPerLogicalName(t *testing.T) {
	if PayloadSeed(42, "img-0001") == PayloadSeed(42, "img-0002") {
		t.Error("payloadSeed collided for two distinct logical names")
	}
	if PayloadSeed(42, "img-0001") == PayloadSeed(43, "img-0001") {
		t.Error("payloadSeed did not vary with the plan seed")
	}
}

// TestPayloadSeedIsStable asserts the same (plan seed, logical name) pair always
// derives the same seed, so a scenario's uploads are byte-identical run to run.
func TestPayloadSeedIsStable(t *testing.T) {
	const seed, name = 42, "img-0001"
	first := PayloadSeed(seed, name)
	if second := PayloadSeed(seed, name); first != second {
		t.Errorf("payloadSeed unstable: %d != %d", first, second)
	}
}
