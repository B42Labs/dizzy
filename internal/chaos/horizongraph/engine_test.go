package horizongraph

import (
	"context"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	horizonexec "github.com/B42Labs/dizzy/internal/horizon/executor"
)

// fakeClock is a virtual clock: Sleep advances time instantly, so the
// scheduler emits a deterministic number of ticks while the operations run
// on real goroutines.
type fakeClock struct{ cur time.Time }

func (c *fakeClock) Now() time.Time { return c.cur }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.cur = c.cur.Add(d)
	return nil
}

func horizonCfg(browse float64) chaos.Config {
	return chaos.Config{
		Duration:    2 * time.Second,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 40 * time.Millisecond,
		MaxParallel: 4,
		ChurnRatio:  0.5,
		TargetFill:  0.7,
		ResizeRatio: browse,
		Concurrency: 8,
		Classify:    Classify,
	}
}

// TestChurnStaysInEnvelopeAndLogsOut asserts a churn run logs in, browses
// and logs out, never holds more sessions logged in than the plan has, and
// leaves LogoutAll exactly the sessions still live.
func TestChurnStaysInEnvelopeAndLogsOut(t *testing.T) {
	d := &fakeDashboard{}
	p := testPlan(5)
	nodes, sessions, err := Build(p, testTarget(d), &horizonexec.Provisioned{}, "run1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r, err := chaos.Run(context.Background(), nodes, p.Seed, horizonCfg(0.5), &fakeClock{cur: time.Unix(0, 0)})
	if err != nil {
		t.Fatalf("chaos.Run: %v", err)
	}
	if r.Creates == 0 || r.Deletes == 0 || r.Mutates == 0 {
		t.Errorf("result: %d creates, %d deletes, %d mutates, want each above 0", r.Creates, r.Deletes, r.Mutates)
	}
	if d.maxLogged > len(p.Sessions) {
		t.Errorf("held %d sessions logged in at once, above the %d of the plan", d.maxLogged, len(p.Sessions))
	}
	if d.count("login") != r.Creates {
		t.Errorf("sent %d logins for %d creates", d.count("login"), r.Creates)
	}
	if got := sessions.Len(); got != len(r.Created) {
		t.Errorf("graph holds %d sessions, the engine reports %d live", got, len(r.Created))
	}
	live := d.loggedIn
	n, err := sessions.LogoutAll(context.Background(), time.Second)
	if err != nil || n != live {
		t.Errorf("LogoutAll() = %d, %v, want %d, nil", n, err, live)
	}
	if d.loggedIn != 0 {
		t.Errorf("%d session(s) still logged in after LogoutAll", d.loggedIn)
	}
}

// TestChurnWithoutBrowsing asserts browse ratio 0 sends no page or switch.
func TestChurnWithoutBrowsing(t *testing.T) {
	d := &fakeDashboard{}
	p := testPlan(3)
	nodes, _, err := Build(p, testTarget(d), &horizonexec.Provisioned{}, "run1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r, err := chaos.Run(context.Background(), nodes, p.Seed, horizonCfg(0), &fakeClock{cur: time.Unix(0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Mutates != 0 || d.count("page") != 0 || d.count("switch") != 0 {
		t.Errorf("browse ratio 0 gave %d mutates, %d pages, %d switches", r.Mutates, d.count("page"), d.count("switch"))
	}
}
