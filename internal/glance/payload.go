package glance

import (
	"hash/fnv"
	"io"
	"math/rand"
)

// payloadReader returns a reader that yields exactly n bytes of deterministic,
// pseudo-random data drawn from seed. math/rand v1's Read is frozen for
// compatibility, so the same seed and size always produce byte-identical data —
// which lets a scenario push an identical synthetic payload every run. The bytes
// are valid raw image data (a raw image is an unstructured byte blob), so no
// format inspection rejects them.
func payloadReader(seed, n int64) io.Reader {
	return io.LimitReader(rand.New(rand.NewSource(seed)), n)
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
