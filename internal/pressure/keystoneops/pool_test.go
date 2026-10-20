package keystoneops

import (
	"context"
	"maps"
	"math/rand"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// TestPoolPeekTakePut checks the pool primitives: peek leaves the entry, take
// removes it, put adds one, and both draws report an empty pool.
func TestPoolPeekTakePut(t *testing.T) {
	var p pool
	rng := rand.New(rand.NewSource(1))
	if _, ok := p.peek(rng); ok {
		t.Error("peek on an empty pool reported an entry")
	}
	if _, ok := p.take(rng); ok {
		t.Error("take on an empty pool reported an entry")
	}

	for i, tok := range []string{"a", "b", "c"} {
		p.put(entry{token: tok, pair: i})
	}
	e, ok := p.peek(rng)
	if !ok || p.len() != 3 {
		t.Fatalf("peek = %v, %v with %d entries left, want an entry and 3 left", e, ok, p.len())
	}
	taken := make(map[string]bool)
	for range 3 {
		e, ok := p.take(rng)
		if !ok {
			t.Fatal("take reported an empty pool too early")
		}
		if taken[e.token] {
			t.Fatalf("take returned %q twice", e.token)
		}
		taken[e.token] = true
	}
	if p.len() != 0 || len(taken) != 3 {
		t.Errorf("after three takes: %d left, %d distinct taken; want 0 and 3", p.len(), len(taken))
	}
}

// TestPoolReplace checks that replace swaps an entry in place and leaves the
// pool unchanged when the entry is gone.
func TestPoolReplace(t *testing.T) {
	var p pool
	p.put(entry{token: "a", pair: 0})
	p.put(entry{token: "b", pair: 1})
	if !p.replace(entry{token: "b"}, entry{token: "b2", pair: 1}) {
		t.Error("replace did not find b")
	}
	if p.replace(entry{token: "gone"}, entry{token: "x"}) {
		t.Error("replace found an entry that is not in the pool")
	}
	if got := []entry{p.entries[0], p.entries[1]}; got[0].token != "a" || got[1].token != "b2" || p.len() != 2 {
		t.Errorf("entries = %v, want a and b2 in place", p.entries)
	}
}

// TestRefreshDue drives the refresher's step by hand on a pool of tokens that
// live 40 seconds: the margin is 10 seconds, so 20 seconds after the issue
// nothing is due, 31 seconds after it every entry is replaced by a token for
// the same pair, and a failing issue keeps the old entry. How many issues a
// refresh runs at once is TestRefreshDueBoundsConcurrency's subject.
func TestRefreshDue(t *testing.T) {
	ctx := context.Background()
	s, f := primedSet(t, 40*time.Second, 4, churnOn)
	// Stop the refresher goroutine, so only the test's own calls refresh.
	s.Close()
	issuedAt := f.clock.Now()
	if s.margin != 10*time.Second {
		t.Fatalf("margin = %s, want 10s (a quarter of the 40s lifetime)", s.margin)
	}
	before := poolTokens(s)

	t.Run("nothing due before the margin", func(t *testing.T) {
		m := f.mark()
		s.refreshDue(ctx, issuedAt.Add(20*time.Second))
		if calls := f.since(m); len(calls) != 0 {
			t.Errorf("calls = %+v, want none", calls)
		}
	})

	t.Run("a failing issue keeps the old entry", func(t *testing.T) {
		logs := captureLogs(t)
		f.fail(kindIssue, errFake)
		defer f.fail(kindIssue, nil)
		m := f.mark()
		s.refreshDue(ctx, issuedAt.Add(31*time.Second))
		if n := len(f.since(m)); n != 4 {
			t.Errorf("%d issue attempts, want 4", n)
		}
		if after := poolTokens(s); len(after) != 4 || !sameKeys(after, before) {
			t.Errorf("pool = %v, want the old entries %v", after, before)
		}
		out := logs.String()
		if !strings.Contains(out, "token pool refresh failed; the entry is retried on the next tick") {
			t.Errorf("log = %q, want the refresh warning", out)
		}
		if strings.Contains(out, "tok-") || strings.Contains(out, "pw-") {
			t.Errorf("log = %q carries a token or a password", out)
		}
	})

	t.Run("every entry within the margin is replaced", func(t *testing.T) {
		now := issuedAt.Add(31 * time.Second)
		f.clock.Set(now)
		m := f.mark()
		s.refreshDue(ctx, now)
		calls := f.since(m)
		if len(calls) != 4 {
			t.Fatalf("%d calls, want 4 issues", len(calls))
		}
		after := poolTokens(s)
		if len(after) != 4 {
			t.Errorf("pool size = %d, want 4", len(after))
		}
		pairsBefore, pairsAfter := make(map[int]int), make(map[int]int)
		for tok, pairIdx := range before {
			pairsBefore[pairIdx]++
			if _, ok := after[tok]; ok {
				t.Errorf("old token %s is still in the pool", tok)
			}
		}
		for _, pairIdx := range after {
			pairsAfter[pairIdx]++
		}
		if !sameCounts(pairsBefore, pairsAfter) {
			t.Errorf("tokens per pair = %v, want %v", pairsAfter, pairsBefore)
		}
		for _, e := range poolEntries(s) {
			if want := now.Add(40 * time.Second); !e.expiresAt.Equal(want) {
				t.Errorf("entry expires at %s, want %s", e.expiresAt, want)
			}
		}

		// The new tokens are not due until they come within the margin.
		m = f.mark()
		s.refreshDue(ctx, now.Add(25*time.Second))
		if calls := f.since(m); len(calls) != 0 {
			t.Errorf("fresh tokens refreshed again: %+v", calls)
		}
	})
}

// TestRefreshDueFanTokens checks that the refresh covers the fan users'
// tokens: the pool tokens live an hour, which sets a 5-minute margin, and the
// fan tokens 40 minutes, so 36 minutes after priming only the fan tokens are
// due. The refresh reissues each for its fan pair, replaces it in the fan
// pool, which keeps one entry per fan user, and leaves the pool as it was.
func TestRefreshDueFanTokens(t *testing.T) {
	ctx := context.Background()
	f := newFake(newFakeClock(), time.Hour)
	// Prime with concurrency 1 issues the two pool tokens and then the two
	// fan tokens, in that order.
	f.setIssueTiming(0, time.Hour, time.Hour, 40*time.Minute, 40*time.Minute)
	s := buildFanSet(t, f, fanPlan(5, []string{"proj-0002"}, []string{"proj-0004"}), fixtureOptions(2, churnOn))
	if err := s.Prime(ctx, 1); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	// Stop the refresher goroutine, so only the test's own call refreshes.
	s.Close()
	if s.margin != maxRefreshMargin {
		t.Fatalf("margin = %s, want %s from the hour-long first pool token", s.margin, maxRefreshMargin)
	}
	poolBefore, fanBefore := poolTokens(s), fanTokens(s)

	now := f.clock.Now().Add(36 * time.Minute)
	f.clock.Set(now)
	m := f.mark()
	s.refreshDue(ctx, now)

	type issueArgs struct{ user, project string }
	got := make(map[issueArgs]int)
	for _, c := range f.since(m) {
		got[issueArgs{c.user, c.project}]++
	}
	if want := map[issueArgs]int{{"fan-0001", "proj-0002"}: 1, {"fan-0002", "proj-0004"}: 1}; !maps.Equal(got, want) {
		t.Errorf("refresh issues = %v, want one per fan user for its pair %v", got, want)
	}
	if after := poolTokens(s); !maps.Equal(after, poolBefore) {
		t.Errorf("pool = %v after a refresh of the fan tokens, want %v", after, poolBefore)
	}
	after := fanTokens(s)
	if len(after) != len(s.fan) {
		t.Fatalf("fan pool = %v, want one entry per fan user", after)
	}
	for tok := range fanBefore {
		if _, ok := after[tok]; ok {
			t.Errorf("old fan token %s is still in the fan pool", tok)
		}
	}
	pairs := make(map[int]bool)
	for _, pairIdx := range after {
		pairs[pairIdx] = true
	}
	for _, u := range s.fan {
		if !pairs[u.pair] {
			t.Errorf("fan pool %v has no entry for fan user %q's pair %d", after, u.name, u.pair)
		}
	}
	for _, e := range fanEntries(s) {
		if want := now.Add(time.Hour); !e.expiresAt.Equal(want) {
			t.Errorf("fan entry expires at %s, want %s", e.expiresAt, want)
		}
	}
}

// TestRefreshSchedule checks the refresher's timing: the margin is a quarter
// of the token lifetime and at most five minutes, the refresher looks every
// half margin, and a lifetime too short for a positive interval, as a token
// without expiry has, starts no refresher.
func TestRefreshSchedule(t *testing.T) {
	tests := []struct {
		lifetime, margin, interval time.Duration
		ok                         bool
	}{
		{40 * time.Second, 10 * time.Second, 5 * time.Second, true},
		{400 * time.Millisecond, 100 * time.Millisecond, 50 * time.Millisecond, true},
		{20 * time.Minute, 5 * time.Minute, 150 * time.Second, true},
		{24 * time.Hour, 5 * time.Minute, 150 * time.Second, true},
		{8 * time.Nanosecond, 2 * time.Nanosecond, time.Nanosecond, true},
		{7 * time.Nanosecond, 0, 0, false},
		{0, 0, 0, false},
		{-time.Minute, 0, 0, false},
	}
	for _, tt := range tests {
		margin, interval, ok := refreshSchedule(tt.lifetime)
		if ok != tt.ok {
			t.Errorf("refreshSchedule(%s) ok = %v, want %v", tt.lifetime, ok, tt.ok)
			continue
		}
		if ok && (margin != tt.margin || interval != tt.interval) {
			t.Errorf("refreshSchedule(%s) = margin %s, interval %s; want %s, %s", tt.lifetime, margin, interval, tt.margin, tt.interval)
		}
	}
}

// TestRefreshDueBoundsConcurrency checks that a refresh reissues the due
// entries with exactly as many issues in flight as Prime had, 2 here, and
// that the pool keeps every old entry while their replacements are in flight.
// The fake holds every refresh issue at its gate and the test lets them
// through one at a time. In the synctest bubble, synctest.Wait returns once
// every goroutine is blocked, so the refresh has started every issue it can
// start before the test counts them: a refresh that reissues one entry at a
// time has one in flight, and one without a bound has all six.
func TestRefreshDueBoundsConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const tokens, concurrency = 6, 2
		s, f := primedSet(t, 40*time.Second, tokens, churnOn) // primed with concurrency 2
		// Stop the refresher goroutine, so only the test's own call refreshes.
		s.Close()
		before := poolTokens(s)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		f.gateIssues(t)
		refreshed := make(chan struct{})
		go func() {
			defer close(refreshed)
			s.refreshDue(ctx, f.clock.Now().Add(31*time.Second))
		}()

		validate := operation(t, s, OpValidate)
		rng := rand.New(rand.NewSource(1))
		for released := range tokens {
			synctest.Wait()
			if n, want := f.issuesInFlight(), min(concurrency, tokens-released); n != want {
				t.Fatalf("%d refresh issues in flight with %d of %d let through, want %d (Prime's concurrency of %d)", n, released, tokens, want, concurrency)
			}
			if released == 0 {
				// Two replacements are in flight: the pool still holds
				// every old entry, and validate keeps drawing from it.
				if got := poolTokens(s); !maps.Equal(got, before) {
					t.Errorf("pool = %v while the replacements are in flight, want every old entry %v", got, before)
				}
				m := f.mark()
				if err := validate(ctx, rng); err != nil {
					t.Errorf("validate during the refresh: %v", err)
				}
				if calls := f.since(m); len(calls) != 1 || calls[0].kind != kindValidate {
					t.Errorf("calls = %+v, want one validation", calls)
				} else if _, ok := before[calls[0].token]; !ok {
					t.Errorf("validated %q during the refresh, not an old pool token", calls[0].token)
				}
			}
			f.releaseIssue(t)
		}
		synctest.Wait()
		select {
		case <-refreshed:
		default:
			t.Fatal("refreshDue did not return after every issue was let through")
		}

		after := poolTokens(s)
		if len(after) != tokens {
			t.Errorf("pool size = %d after the refresh, want %d", len(after), tokens)
		}
		for tok := range before {
			if _, ok := after[tok]; ok {
				t.Errorf("old token %s is still in the pool", tok)
			}
		}
	})
}

// TestRefreshDropsReplacementOfTakenEntry checks that a refresh never grows
// the pool: an entry that a revoke or an issue takes while its replacement is
// in flight is not replaced, and the replacement is discarded rather than
// added.
func TestRefreshDropsReplacementOfTakenEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := primedSet(t, 40*time.Second, 3, churnOn) // one entry per pool pair
		s.Close()
		before := poolTokens(s)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		f.gateIssues(t)
		m := f.mark()
		refreshed := make(chan struct{})
		go func() {
			defer close(refreshed)
			s.refreshDue(ctx, f.clock.Now().Add(31*time.Second))
		}()

		synctest.Wait()
		held := f.since(m)
		if len(held) == 0 {
			t.Fatal("the refresh made no issue")
		}
		pairIdx := pairIndex(t, s, held[0].user, held[0].project)
		taken := takePair(t, s, pairIdx) // as a revoke or an issue would
		f.openGate()
		synctest.Wait()
		select {
		case <-refreshed:
		default:
			t.Fatal("refreshDue did not return after the gate opened")
		}

		after := poolTokens(s)
		if want := len(before) - 1; len(after) != want {
			t.Errorf("pool size after refresh = %d, want %d (the taken entry's replacement discarded)", len(after), want)
		}
		for tok, p := range after {
			if p == pairIdx {
				t.Errorf("pool holds %s for pair %d, whose entry %s was taken during its refresh", tok, p, taken)
			}
			if _, old := before[tok]; old {
				t.Errorf("old token %s was not refreshed", tok)
			}
		}
	})
}

// TestRefresherTicksAndCloseStopsIt checks the refresher goroutine: with a
// 400ms lifetime it ticks every 50ms, replaces the entries that come within
// the 100ms margin of the clock it reads, and Close stops it and waits for it.
func TestRefresherTicksAndCloseStopsIt(t *testing.T) {
	clock := newFakeClock()
	f := newFake(clock, 400*time.Millisecond)
	s := buildSet(t, f, fixtureOptions(3, churnOn))
	// ticked receives a value whenever the refresher reads the clock, which it
	// does once per tick, before the refresh of that tick.
	ticked := make(chan struct{})
	s.now = func() time.Time {
		select {
		case ticked <- struct{}{}:
		default:
		}
		return clock.Now()
	}
	if err := s.Prime(context.Background(), 2); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	before := poolTokens(s)
	clock.Set(clock.Now().Add(350 * time.Millisecond))

	// The second tick starts only after the refresh of the first returned.
	for i := range 2 {
		select {
		case <-ticked:
		case <-time.After(10 * time.Second):
			t.Fatalf("the refresher did not tick (tick %d)", i+1)
		}
	}
	after := poolTokens(s)
	if len(after) != 3 {
		t.Errorf("pool size = %d, want 3", len(after))
	}
	for tok := range before {
		if _, ok := after[tok]; ok {
			t.Errorf("old token %s was not refreshed", tok)
		}
	}

	s.Close()
	select {
	case <-s.done:
	default:
		t.Error("Close returned before the refresher goroutine")
	}
	s.Close() // a second Close is a no-op
}

// TestCloseBeforePrime checks that Close before Prime does not panic and that
// a Prime after Close starts no refresher.
func TestCloseBeforePrime(t *testing.T) {
	f := newFake(newFakeClock(), time.Hour)
	s := buildSet(t, f, fixtureOptions(2, churnOn))
	s.Close()
	s.Close()
	if err := s.Prime(context.Background(), 2); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if s.done != nil {
		t.Error("Prime after Close started the refresher")
	}
}

// TestNoExpiryStartsNoRefresher checks that a token without an expiry starts
// no refresher and that the warning names no token.
func TestNoExpiryStartsNoRefresher(t *testing.T) {
	logs := captureLogs(t)
	s, _ := primedSet(t, 0, 2, churnOn)
	if s.done != nil {
		t.Error("a pool without token expiry started the refresher")
	}
	out := logs.String()
	if !strings.Contains(out, "token pool refresher not started") {
		t.Errorf("log = %q, want the warning", out)
	}
	if strings.Contains(out, "tok-") {
		t.Errorf("log = %q carries a token", out)
	}
	s.Close()
}

// TestRefreshMarginCap checks that a long token lifetime caps the margin at
// five minutes.
func TestRefreshMarginCap(t *testing.T) {
	s, _ := primedSet(t, 24*time.Hour, 1, churnOn)
	if s.margin != 5*time.Minute {
		t.Errorf("margin = %s, want 5m0s", s.margin)
	}
}

// pairIndex returns the index of s's pool pair (user, project).
func pairIndex(t *testing.T, s *Set, user, project string) int {
	t.Helper()
	for i, p := range s.pairs {
		if p.issue.User == user && p.issue.Project == project {
			return i
		}
	}
	t.Fatalf("no pool pair (%s, %s)", user, project)
	return -1
}

// takePair removes the first pool entry of pair i, as a revoke or an issue
// that drew it would, and returns its token.
func takePair(t *testing.T, s *Set, i int) string {
	t.Helper()
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	for j, e := range s.pool.entries {
		if e.pair == i {
			s.pool.entries = append(s.pool.entries[:j], s.pool.entries[j+1:]...)
			return e.token
		}
	}
	t.Fatalf("the pool holds no entry of pair %d", i)
	return ""
}

// sameKeys reports whether a and b hold the same tokens.
func sameKeys(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// sameCounts reports whether a and b map the same pairs to the same counts.
func sameCounts(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
