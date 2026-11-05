// Package payload derives the deterministic synthetic payloads dizzy writes to a
// cloud. A payload's bytes follow from a seed and a length alone, and Seed
// derives that seed from the plan seed and a resource's logical name, so a
// scenario writes byte-identical data on every run while two resources in one
// plan never share a payload. Every service that writes a payload derives it
// here: a Glance image upload streams it through NewReader, and Bytes returns
// the same bytes whole for a caller that sends them in one request body, such as
// a Barbican secret store, or compares them against what the cloud returns.
package payload

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"sync"
	"time"
)

// releaseTimeout bounds how long a rewind waits for the transport to close the
// previous attempt, so a transport that never does fails the upload rather than
// hanging it.
const releaseTimeout = 30 * time.Second

// payload yields exactly n deterministic bytes drawn from seed and can be
// rewound to its start, which gophercloud does when it repeats an upload
// after re-authenticating. net/http may keep reading the previous attempt after
// RoundTrip returned its 401 and reports that it stopped by closing the body, so
// a rewind waits for that Close.
type payload struct {
	seed, n int64
	r       io.Reader

	mu       sync.Mutex    // guards released against concurrent Close calls
	released chan struct{} // closed once the transport stops reading the current attempt
}

// NewReader returns a reader that yields exactly n bytes of deterministic,
// pseudo-random data drawn from seed. math/rand v1's Read is frozen for
// compatibility, so the same seed and size always produce byte-identical data,
// which lets a scenario write an identical synthetic payload every run. The
// bytes carry no structure, so they are valid raw image data (a raw image is an
// unstructured byte blob) and a valid opaque secret alike, and no format
// inspection rejects them. The only supported seek is a rewind to the start,
// which regenerates the stream from seed instead of buffering it, so a streamed
// upload of any size can be repeated without holding it in memory.
func NewReader(seed, n int64) io.ReadSeekCloser {
	p := &payload{seed: seed, n: n}
	p.rewind()
	return p
}

func (p *payload) Read(b []byte) (int, error) { return p.r.Read(b) }

// Close marks the current attempt released. net/http calls it once it no longer
// reads the body; a repeated call is a no-op.
func (p *payload) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.released:
	default:
		close(p.released)
	}
	return nil
}

// Seek rewinds the payload to its start for Seek(0, io.SeekStart), once the
// current attempt was closed, and rejects every other offset and whence.
func (p *payload) Seek(offset int64, whence int) (int64, error) {
	if offset != 0 || whence != io.SeekStart {
		return 0, fmt.Errorf("payload: seek to offset %d from whence %d is not supported", offset, whence)
	}
	select {
	case <-p.released:
	case <-time.After(releaseTimeout):
		return 0, errors.New("payload: previous upload attempt still reading the body")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rewind()
	return 0, nil
}

// rewind restarts the stream from seed for a new attempt.
func (p *payload) rewind() {
	p.r = io.LimitReader(rand.New(rand.NewSource(p.seed)), p.n)
	p.released = make(chan struct{})
}

// Bytes returns the n bytes NewReader(seed, n) yields, in one slice. It serves
// a caller that sends the payload in a single request body or compares a
// payload read back from the cloud with the one it wrote. Reading from a
// math/rand source never fails, so Bytes has no error to return.
func Bytes(seed, n int64) []byte {
	b, _ := io.ReadAll(NewReader(seed, n))
	return b
}

// Seed derives a resource's payload seed from the plan seed and the resource's
// logical name, so two resources in one plan get distinct payloads while the
// whole plan stays reproducible from its single seed. It XORs the plan seed with
// the FNV-64a hash of the logical name. It is exported so every path that
// writes or checks a payload derives the same seed: the Glance apply executor
// and chaos graph both hand it to the client that uploads, keeping a scenario's
// synthetic payloads byte-identical across both paths.
func Seed(planSeed int64, logical string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(logical))
	return planSeed ^ int64(h.Sum64())
}
