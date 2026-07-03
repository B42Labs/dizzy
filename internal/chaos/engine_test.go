package chaos

import (
	"context"
	"testing"
	"time"
)

// fakeClock is a virtual clock: Sleep advances time instantly and records the
// requested delay, so the schedule is deterministic and the drawn delays can be
// inspected. Only the scheduler goroutine touches it.
type fakeClock struct {
	cur    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{cur: time.Unix(0, 0)} }

func (c *fakeClock) Now() time.Time { return c.cur }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.sleeps = append(c.sleeps, d)
	c.cur = c.cur.Add(d)
	return nil
}

// validConfig is a minimal well-formed churn config the config-validation cases
// mutate one field of at a time.
func validConfig() Config {
	return Config{
		Duration:    2 * time.Second,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 40 * time.Millisecond,
		MaxParallel: 4,
		ChurnRatio:  0.5,
		TargetFill:  0.7,
		Concurrency: 8,
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	// Each case violates exactly one rule of Config.Validate, including the upper
	// ceilings that keep absurd-but-typed operator input from driving the
	// scheduler into runaway fan-out or an overflowed interval span. The config
	// is checked before the nodes are touched, so a nil node slice is enough.
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero duration", func(c *Config) { c.Duration = 0 }},
		{"non-positive min-interval", func(c *Config) { c.MinInterval = 0 }},
		{"min-interval above max-interval", func(c *Config) { c.MinInterval = c.MaxInterval + time.Millisecond }},
		{"max-interval above ceiling", func(c *Config) { c.MaxInterval = maxIntervalCeiling + time.Minute }},
		{"zero max-parallel", func(c *Config) { c.MaxParallel = 0 }},
		{"max-parallel above ceiling", func(c *Config) { c.MaxParallel = maxParallelCeiling + 1 }},
		{"zero concurrency", func(c *Config) { c.Concurrency = 0 }},
		{"churn-ratio above one", func(c *Config) { c.ChurnRatio = 1.5 }},
		{"target-fill below zero", func(c *Config) { c.TargetFill = -0.1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			if _, err := Run(context.Background(), nil, 7, cfg, newFakeClock()); err == nil {
				t.Fatal("expected Run to reject the config, got nil error")
			}
		})
	}
}
