package glance

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

// payloadReader returns a reader that yields exactly n bytes of deterministic,
// pseudo-random data drawn from seed. math/rand v1's Read is frozen for
// compatibility, so the same seed and size always produce byte-identical data —
// which lets a scenario push an identical synthetic payload every run. The bytes
// are valid raw image data (a raw image is an unstructured byte blob), so no
// format inspection rejects them. The only supported seek is a rewind to the
// start, which regenerates the stream from seed instead of buffering it.
func payloadReader(seed, n int64) io.ReadSeekCloser {
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

// PayloadSeed derives the per-image payload seed from the plan seed and the
// image's logical name, so two images in one plan get distinct payloads while the
// whole plan stays reproducible from its single seed. It XORs the plan seed with
// the FNV-64a hash of the logical name. It is exported so the apply executor and
// the chaos graph derive the same seed the client uploads with, keeping a
// scenario's synthetic payloads byte-identical across both paths.
func PayloadSeed(planSeed int64, logical string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(logical))
	return planSeed ^ int64(h.Sum64())
}
