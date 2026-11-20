// Package horizongraph builds the Horizon churn graph the service-neutral
// chaos engine schedules. The churning population is dashboard sessions: one
// parentless node of kind session per plan session. A create opens a session
// and logs in, a mutate browses (loads the session's pages and switches
// project as its plan says), and a delete logs out. The users and projects a
// plan with users logs in as are a scaffold outside the graph: the command
// provisions them once before the churn and deletes them after it, so the
// engine never churns a Keystone resource. A session carries no run
// identifier and cannot be listed, so Sessions keeps every live one and
// LogoutAll logs out what the churn left logged in.
package horizongraph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/B42Labs/dizzy/internal/chaos"
	"github.com/B42Labs/dizzy/internal/horizon"
	horizonexec "github.com/B42Labs/dizzy/internal/horizon/executor"
	"github.com/B42Labs/dizzy/internal/horizon/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Sessions holds the logged-in sessions of a churn run by node key. It is
// safe for concurrent use by the engine's operations.
type Sessions struct {
	mu   sync.Mutex
	live map[string]horizonexec.Session
}

// get returns the session of key, or nil when none is live.
func (s *Sessions) get(key string) horizonexec.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[key]
}

// put records sess as the live session of key.
func (s *Sessions) put(key string, sess horizonexec.Session) {
	s.mu.Lock()
	s.live[key] = sess
	s.mu.Unlock()
}

// drop forgets the session of key and closes it.
func (s *Sessions) drop(key string) {
	s.mu.Lock()
	sess := s.live[key]
	delete(s.live, key)
	s.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
}

// Len returns the number of sessions the graph holds.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// LogoutAll logs out every session the graph still holds, in key order,
// each through WithRetry under opTimeout, closes it and forgets it. It
// returns how many it logged out and the first error. With no live session
// it returns 0, nil.
func (s *Sessions) LogoutAll(ctx context.Context, opTimeout time.Duration) (int, error) {
	s.mu.Lock()
	keys := make([]string, 0, len(s.live))
	for k := range s.live {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	slices.Sort(keys)

	var (
		n     int
		first error
	)
	for _, k := range keys {
		sess := s.get(k)
		if sess == nil {
			continue
		}
		if sess.LoggedIn() {
			if err := horizonexec.WithRetry(ctx, opTimeout, sess.Logout); err != nil {
				if first == nil {
					first = fmt.Errorf("logging out %s: %w", k, err)
				}
			} else {
				n++
			}
		}
		s.drop(k)
	}
	return n, first
}

// Build turns a Horizon plan into the churn graph: one parentless node of
// kind session per plan session, in plan order, keyed by the session's name.
// Create opens a session through t.Open and logs in with
// horizonexec.CredentialsFor, Mutate browses with horizonexec.Browse, and
// Delete logs out and closes the session; each runs through
// horizonexec.WithRetry under opTimeout. A created session is recorded as
// resource.Resource{Kind: session, Logical: name, Name:
// dizzyhorizon-<runID>-<name>, ID: name}. Mutate and Delete of a session
// that is not logged in send nothing. The plan is validated first.
func Build(p *plan.Plan, t horizonexec.Target, prov *horizonexec.Provisioned, runID string, opTimeout time.Duration) ([]chaos.Node, *Sessions, error) {
	if err := p.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid plan: %w", err)
	}
	sessions := &Sessions{live: make(map[string]horizonexec.Session, len(p.Sessions))}

	nodes := make([]chaos.Node, 0, len(p.Sessions))
	for _, ps := range p.Sessions {
		key := ps.Name
		nodes = append(nodes, chaos.Node{
			Key:  key,
			Kind: horizon.KindSession,
			Create: func(ctx context.Context, _ map[string]string) (resource.Resource, error) {
				sess := t.Open()
				cred := horizonexec.CredentialsFor(t, prov, ps)
				if err := horizonexec.WithRetry(ctx, opTimeout, func(ctx context.Context) error { return sess.Login(ctx, cred) }); err != nil {
					sess.Close()
					return resource.Resource{}, err
				}
				sessions.put(key, sess)
				return resource.Resource{
					Kind:    horizon.KindSession,
					Logical: key,
					Name:    "dizzyhorizon-" + runID + "-" + key,
					ID:      key,
				}, nil
			},
			Mutate: func(ctx context.Context, _ map[string]string, _ resource.Resource) error {
				sess := sessions.get(key)
				if sess == nil || !sess.LoggedIn() {
					return nil
				}
				return horizonexec.Browse(ctx, sess, t, prov, p, ps, opTimeout)
			},
			Delete: func(ctx context.Context, _ map[string]string, _ resource.Resource) error {
				sess := sessions.get(key)
				if sess == nil {
					return nil
				}
				if !sess.LoggedIn() {
					sessions.drop(key)
					return nil
				}
				if err := horizonexec.WithRetry(ctx, opTimeout, sess.Logout); err != nil {
					// Keep it, so a retried delete or LogoutAll logs it out.
					return err
				}
				sessions.drop(key)
				return nil
			},
		})
	}
	return nodes, sessions, nil
}

// Classify labels an operation error for the per-bucket error breakdown:
// "" for nil, then canceled, timeout, rejected, mismatch, transient (a
// retryable failure) and other, checked in that order.
func Classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, horizon.ErrRejected):
		return "rejected"
	case errors.Is(err, horizon.ErrMismatch):
		return "mismatch"
	case horizon.IsRetryable(err):
		return "transient"
	default:
		return "other"
	}
}
