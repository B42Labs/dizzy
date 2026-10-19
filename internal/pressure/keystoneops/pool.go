package keystoneops

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

// ErrPoolEmpty is returned by an operation that needs a pool token when the
// pool holds none, because every entry was taken by other workers or lost to
// failed reissues. Classify labels it "pool-empty"; callers match it with
// errors.Is.
var ErrPoolEmpty = errors.New("keystoneops: token pool is empty")

// entry is one pool token: the token id, the expiry Keystone reported for it
// and the index of the (user, project) pair it was issued for, so a revoke, an
// issue or the refresher can obtain a replacement for the same pair. The token
// lives only in process memory: it is never logged, put into an error text or
// returned from an exported function.
type entry struct {
	token     string
	expiresAt time.Time
	pair      int
}

// pool is the set of live tokens the operations draw from. It is safe for
// concurrent use by the workers and the refresher. The order of its entries
// carries no meaning; every draw picks one uniformly at random from the
// caller's generator.
type pool struct {
	mu      sync.Mutex
	entries []entry
}

// peek returns a random entry without removing it, for the operations that
// only read a token. ok is false when the pool is empty.
func (p *pool) peek(rng *rand.Rand) (e entry, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return entry{}, false
	}
	return p.entries[rng.Intn(len(p.entries))], true
}

// take removes a random entry and returns it, for the operations that replace
// a token, so two workers never revoke or replace the same entry. ok is false
// when the pool is empty.
func (p *pool) take(rng *rand.Rand) (e entry, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.entries)
	if n == 0 {
		return entry{}, false
	}
	i := rng.Intn(n)
	e = p.entries[i]
	p.entries[i] = p.entries[n-1]
	p.entries[n-1] = entry{}
	p.entries = p.entries[:n-1]
	return e, true
}

// put adds e to the pool.
func (p *pool) put(e entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, e)
}

// len returns the number of entries the pool holds now.
func (p *pool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// expiringBefore returns a copy of every entry whose expiry lies before t, the
// entries the refresher replaces. The entries stay in the pool, so the
// operations keep drawing them while their replacements are issued.
func (p *pool) expiringBefore(t time.Time) []entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	var due []entry
	for _, e := range p.entries {
		if e.expiresAt.Before(t) {
			due = append(due, e)
		}
	}
	return due
}

// replace swaps the entry holding old's token for next, in place, and reports
// whether it found it. It finds nothing when a revoke or an issue took the
// entry in the meantime; the caller then discards next, so a refresh never
// grows the pool.
func (p *pool) replace(old, next entry) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.entries {
		if p.entries[i].token == old.token {
			p.entries[i] = next
			return true
		}
	}
	return false
}
