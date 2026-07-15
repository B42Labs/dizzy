package glance

import (
	"bytes"
	"io"
	"testing"
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

// TestPayloadSeedVariesPerLogicalName asserts two logical names under one plan
// seed derive distinct payload seeds, so two images never share a payload, while
// the same (seed, name) pair is stable.
func TestPayloadSeedVariesPerLogicalName(t *testing.T) {
	if PayloadSeed(42, "img-0001") == PayloadSeed(42, "img-0002") {
		t.Error("payloadSeed collided for two distinct logical names")
	}
	if PayloadSeed(42, "img-0001") != PayloadSeed(42, "img-0001") {
		t.Error("payloadSeed is not stable for the same plan seed and name")
	}
	if PayloadSeed(42, "img-0001") == PayloadSeed(43, "img-0001") {
		t.Error("payloadSeed did not vary with the plan seed")
	}
}
