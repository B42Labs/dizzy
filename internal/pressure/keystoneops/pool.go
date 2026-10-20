package keystoneops

import (
	"errors"
	"math/rand"
	"slices"
	"sync"
	"time"
)

// ErrPoolEmpty is returned by an operation that needs a pool token when the
// pool holds none, because every entry was taken by other workers or lost to
// failed reissues. Classify labels it "pool-empty"; callers match it with
// errors.Is.
var ErrPoolEmpty = errors.New("keystoneops: token pool is empty")

// revokedTokenAge is how long after its revocation a pool token may serve as a
// bad token: keystonemiddleware's default token cache time. A younger revoked
// token may still sit in a service's cache, which is what the propagation
// probe measures, so a bad_token draw never sends one.
const revokedTokenAge = 5 * time.Minute

// maxRevokedTokens is how many revoked tokens the set keeps for bad_token; an
// add beyond it drops the oldest, so the memory does not grow with the run.
const maxRevokedTokens = 256

// entry is one pool token: the token id, the expiry Keystone reported for it,
// the index of the (user, project) pair it was issued for, so a revoke, an
// issue or the refresher can obtain a replacement for the same pair, and the
// service endpoints of the token's own catalog. The token lives only in
// process memory: it is never logged, put into an error text or returned from
// an exported function.
type entry struct {
	token     string
	expiresAt time.Time
	pair      int
	// endpoints is IssuedToken.Endpoints of the issue that produced the
	// entry, so a refreshed or reissued entry carries the endpoints of its
	// own issue. It is nil when the client resolves no endpoints, and it is
	// only read once the entry exists.
	endpoints map[string]string
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

// revokedToken is one pool token the set revoked and the instant of the
// revocation on the set's clock.
type revokedToken struct {
	token string
	at    time.Time
}

// revokedTokens holds the most recent maxRevokedTokens pool tokens the revoke
// operation and the propagation probe revoked, for bad_token to send as a
// token the services have to refuse. Its instants come from the set's clock,
// the one the refresher reads and tests move. It is safe for concurrent use by
// the workers and the propagation probes. Like the pool, it never hands a
// token to a log line or an error text.
type revokedTokens struct {
	mu      sync.Mutex
	entries []revokedToken // in add order, oldest first
}

// add records token as revoked at at and drops the oldest entry once the ring
// holds more than maxRevokedTokens.
func (r *revokedTokens) add(token string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, revokedToken{token: token, at: at})
	if over := len(r.entries) - maxRevokedTokens; over > 0 {
		r.entries = slices.Delete(r.entries, 0, over)
	}
}

// pick returns a token drawn uniformly from rng among the entries revoked at
// least revokedTokenAge before now, and false, without drawing, when there is
// none. It counts the eligible entries first and then walks to the drawn one,
// so a draw allocates nothing.
func (r *revokedTokens) pick(rng *rand.Rand, now time.Time) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	eligible := 0
	for _, t := range r.entries {
		if t.oldEnough(now) {
			eligible++
		}
	}
	if eligible == 0 {
		return "", false
	}
	k := rng.Intn(eligible)
	for _, t := range r.entries {
		if !t.oldEnough(now) {
			continue
		}
		if k == 0 {
			return t.token, true
		}
		k--
	}
	return "", false // not reached: k < eligible
}

// len returns the number of revoked tokens the ring holds now.
func (r *revokedTokens) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// oldEnough reports whether t was revoked at least revokedTokenAge before now,
// so no service still holds it in its token cache.
func (t revokedToken) oldEnough(now time.Time) bool {
	return !t.at.Add(revokedTokenAge).After(now)
}
