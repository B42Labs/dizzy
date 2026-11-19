package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/B42Labs/dizzy/internal/c5c3"
	"github.com/B42Labs/dizzy/internal/resource"
)

// Cleaner is the teardown surface Cleanup drives: discover a run's orders by
// their dizzy/run label, read the credentials their Secrets deliver, wait for a
// user order's password generation to settle and read its status, delete an
// order and wait for it to be gone, observe an order that stays, and check that
// a deleted order's Secret is gone and Keystone rejects its credential. Like
// Orders it is a ports-and-adapters seam to the cluster and to Keystone:
// *c5c3.Client satisfies it in production and a fake satisfies it in tests.
type Cleaner interface {
	ListByRun(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error)
	Status(ctx context.Context, r resource.Resource) (c5c3.OrderStatus, error)
	LookupCredential(ctx context.Context, order resource.Resource) (c5c3.Credential, bool, error)
	WaitGenerationSettled(ctx context.Context, r resource.Resource) error
	Delete(ctx context.Context, r resource.Resource) error
	WaitGone(ctx context.Context, r resource.Resource) error
	Observe(ctx context.Context, r resource.Resource) (string, bool, error)
	WaitSecretGone(ctx context.Context, order resource.Resource) error
	Reject(ctx context.Context, kind resource.Kind, cred c5c3.Credential) error
}

// The production *c5c3.Client must satisfy the seam.
var _ Cleaner = (*c5c3.Client)(nil)

// deleteOrder lists the order kinds in the order Cleanup deletes them, the
// order the operator's holds require. An application credential order holds
// its user, its project and their role assignments; a role assignment holds
// its user and its project. The catalog entry holds nothing and is held by
// nothing, so its place only keeps the order fixed.
var deleteOrder = []resource.Kind{
	c5c3.KindApplicationCredential,
	c5c3.KindRoleAssignment,
	c5c3.KindCatalogEntry,
	c5c3.KindProject,
	c5c3.KindUser,
}

// credentialKinds are the order kinds that deliver a Secret, in the order
// Cleanup looks their credentials up and checks them.
var credentialKinds = []resource.Kind{c5c3.KindApplicationCredential, c5c3.KindUser}

// delivered is a credential read before the delete from the Secret beside a
// user or application credential order.
type delivered struct {
	order resource.Resource
	cred  c5c3.Credential
}

// remaining is an order Cleanup could not confirm gone in its delete pass:
// its Delete failed, its wait for it to be gone ended first, or its wait was
// skipped. err is the Delete's error, nil when the Delete was sent.
type remaining struct {
	order resource.Resource
	err   error
}

// Cleanup deletes every order a run created, strictly by run identity, and
// returns the number of orders confirmed gone. It runs six steps:
//
//  1. It lists each of the five order kinds by the run's label and unions the
//     result with the run record's orders of that kind, deduplicated by id; a
//     record entry of another kind or with an empty id is dropped. A run with
//     no order deletes nothing and returns 0, nil.
//  2. It reads the Secret beside each user and application credential order.
//     An order whose Secret is missing or cannot be parsed has nothing to
//     check in step 6.
//  3. When an application credential order is among them, it waits up to
//     readyTimeout for each user order's password generation to settle, since
//     deleting a credential order while its user's password rotates can strand
//     the credential order's finalizer. A user order that is already gone
//     counts as settled; one that does not settle in time is logged as a
//     warning and the cleanup goes on.
//  4. It deletes kind by kind in deleteOrder. Within a kind the Delete calls
//     run in parallel up to concurrency, each retried on transient errors
//     under opTimeout, and then the waits for the orders to be gone run in
//     parallel, each under goneTimeout. The next kind starts once the previous
//     one is gone. A 404 to the Delete is success.
//  5. Once an order is not confirmed gone, because its Delete failed or its
//     wait ended first, the Delete calls of the remaining kinds are still
//     sent, so the orders it holds go once its hold lifts, but their waits are
//     skipped. Each order not confirmed gone is then observed under opTimeout:
//     one found gone counts as gone, and the others are named in the returned
//     error with what the operator reports for them, such as
//     ReferencedByApplicationCredentials.
//  6. For each credential of step 2 whose order is confirmed gone, it waits up
//     to opTimeout for the garbage collector to remove the Secret, then sends
//     one login under opTimeout that Keystone must reject. A Secret still
//     present is an error wrapping c5c3.ErrMismatch, an accepted credential
//     one wrapping c5c3.ErrAccepted. Every credential is checked before the
//     first error is returned; the later ones are logged.
//
// The count includes an order that was already gone when its Delete was sent.
// A listing error stops the cleanup before anything is deleted. The errors of
// steps 2, 5 and 6 do not stop it: they are joined and returned once step 6
// has run. Cancelling ctx stops it after the call in flight and returns
// ctx.Err() with the count so far. Running Cleanup again is safe at every
// point: it rediscovers the orders by label and a 404 is success.
//
// An empty runID is refused before any request, as the other services'
// cleanups refuse it, so a cleanup never selects by an empty identity.
func Cleanup(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, concurrency int, opTimeout, readyTimeout, goneTimeout time.Duration) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("cleanup: empty run id; refusing to delete by empty identity")
	}

	found, err := discover(ctx, c, runID, recorded, opTimeout)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, list := range found {
		total += len(list)
	}
	if total == 0 {
		return 0, nil
	}

	creds, lookupErr := lookupCredentials(ctx, c, found, opTimeout)
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if len(found[c5c3.KindApplicationCredential]) > 0 {
		settleUsers(ctx, c, found[c5c3.KindUser], opTimeout, readyTimeout)
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}

	gone, left, err := deleteKinds(ctx, c, found, concurrency, opTimeout, goneTimeout)
	deleted := len(gone)
	if err != nil {
		return deleted, err
	}

	stillErr := observeRemaining(ctx, c, left, opTimeout, gone)
	deleted = len(gone)
	if err := ctx.Err(); err != nil {
		return deleted, err
	}

	verifyErr := verifyCredentials(ctx, c, creds, gone, opTimeout)
	if err := ctx.Err(); err != nil {
		return deleted, err
	}
	return deleted, errors.Join(stillErr, verifyErr, lookupErr)
}

// discover is step 1: it lists each order kind by the run's label, each
// listing once under opTimeout, and unions the result with the record's orders
// of that kind. The first listing error is returned.
func discover(ctx context.Context, c Cleaner, runID string, recorded []resource.Resource, opTimeout time.Duration) (map[resource.Kind][]resource.Resource, error) {
	found := make(map[resource.Kind][]resource.Resource, len(deleteOrder))
	for _, kind := range deleteOrder {
		var listed []resource.Resource
		err := once(ctx, opTimeout, func(ctx context.Context) error {
			var err error
			listed, err = c.ListByRun(ctx, kind, runID)
			return err
		})
		if err != nil {
			return nil, err
		}
		found[kind] = unionOfKind(kind, listed, recorded)
	}
	return found, nil
}

// lookupCredentials is step 2: it reads the Secret beside each user and
// application credential order once under opTimeout and returns the
// credentials there are to check after the delete. A failed read is logged and
// leaves that order unchecked; the first such error is returned for Cleanup to
// report once it is done.
func lookupCredentials(ctx context.Context, c Cleaner, found map[resource.Kind][]resource.Resource, opTimeout time.Duration) ([]delivered, error) {
	var (
		creds    []delivered
		firstErr error
	)
	for _, kind := range credentialKinds {
		for _, r := range found[kind] {
			var (
				cred c5c3.Credential
				ok   bool
			)
			err := once(ctx, opTimeout, func(ctx context.Context) error {
				var err error
				cred, ok, err = c.LookupCredential(ctx, r)
				return err
			})
			switch {
			case err != nil:
				if ctx.Err() != nil {
					return nil, nil
				}
				slog.Warn("cannot read the delivered secret; its credential is not checked after the delete",
					"kind", r.Kind, "id", r.ID, "error", err)
				if firstErr == nil {
					firstErr = fmt.Errorf("reading the delivered secret of order %s: %w", r.ID, err)
				}
			case ok:
				creds = append(creds, delivered{order: r, cred: cred})
			}
		}
	}
	return creds, firstErr
}

// settleUsers is step 3: it waits, for every user order in parallel, up to
// readyTimeout for the order's status generation to catch up with its spec
// generation. A user order that is already gone counts as settled. One that
// does not settle in time is logged as a warning, with its generations when a
// status read under opTimeout gives them, and the cleanup goes on.
func settleUsers(ctx context.Context, c Cleaner, users []resource.Resource, opTimeout, readyTimeout time.Duration) {
	_, _ = each(ctx, users, len(users), func(ctx context.Context, r resource.Resource) error {
		err := bounded(ctx, readyTimeout, func(ctx context.Context) error { return c.WaitGenerationSettled(ctx, r) })
		if err == nil || c5c3.IsNotFound(err) || ctx.Err() != nil {
			return nil
		}
		attrs := []any{"id", r.ID, "timeout", readyTimeout, "error", err}
		var st c5c3.OrderStatus
		if serr := once(ctx, opTimeout, func(ctx context.Context) error {
			var err error
			st, err = c.Status(ctx, r)
			return err
		}); serr == nil {
			attrs = append(attrs, "specGeneration", st.SpecGeneration, "generation", st.Generation)
		}
		slog.Warn("the user order's password generation did not settle; deleting its application credentials anyway, "+
			"whose teardown can strand while the password rotates", attrs...)
		return nil
	})
}

// deleteKinds is steps 4 and 5's delete pass: it deletes the orders kind by
// kind in deleteOrder and, until an order is not confirmed gone, waits for
// each kind to be gone before the next. It returns the ids confirmed gone and
// the orders that are not, in deletion order. Its error is ctx's when ctx
// ended.
func deleteKinds(ctx context.Context, c Cleaner, found map[resource.Kind][]resource.Resource, concurrency int, opTimeout, goneTimeout time.Duration) (map[string]bool, []remaining, error) {
	gone := map[string]bool{}
	var left []remaining
	for _, kind := range deleteOrder {
		targets := found[kind]
		held := len(left) > 0

		delErrs, err := each(ctx, targets, concurrency, func(ctx context.Context, r resource.Resource) error {
			return deleteOne(ctx, c, r, opTimeout)
		})
		if err != nil {
			return gone, left, err
		}
		var sent []resource.Resource
		for i, r := range targets {
			if delErrs[i] != nil {
				left = append(left, remaining{order: r, err: delErrs[i]})
				continue
			}
			sent = append(sent, r)
		}

		if held {
			// An earlier kind is still present and may hold these orders, so a
			// wait could only run into goneTimeout.
			for _, r := range sent {
				left = append(left, remaining{order: r})
			}
			continue
		}
		waitErrs, err := each(ctx, sent, len(sent), func(ctx context.Context, r resource.Resource) error {
			return bounded(ctx, goneTimeout, func(ctx context.Context) error { return c.WaitGone(ctx, r) })
		})
		if err != nil {
			return gone, left, err
		}
		for i, r := range sent {
			if waitErrs[i] != nil {
				slog.Warn("deleted order is still present; the deletes of the remaining kinds are sent without waiting",
					"kind", r.Kind, "id", r.ID, "timeout", goneTimeout, "error", waitErrs[i])
				left = append(left, remaining{order: r})
				continue
			}
			gone[r.ID] = true
		}
	}
	return gone, left, nil
}

// deleteOne deletes a single order, retrying transient errors under opTimeout
// and treating an order that is already gone (a 404) as success.
func deleteOne(ctx context.Context, c Cleaner, r resource.Resource, opTimeout time.Duration) error {
	slog.Info("deleting resource", "kind", r.Kind, "id", r.ID)
	err := WithRetry(ctx, opTimeout, func(ctx context.Context) error { return c.Delete(ctx, r) })
	if err != nil && !c5c3.IsNotFound(err) {
		slog.Warn("delete failed; the deletes of the remaining kinds are sent without waiting",
			"kind", r.Kind, "id", r.ID, "error", err)
		return err
	}
	return nil
}

// observeRemaining is step 5's report: it observes each order the delete pass
// could not confirm gone, once under opTimeout. An order found gone is added
// to gone. The others are named in the returned error with what the operator
// reports for them, joined with the error of each one's failed Delete. It
// stops when ctx ends, which the caller checks.
func observeRemaining(ctx context.Context, c Cleaner, left []remaining, opTimeout time.Duration, gone map[string]bool) error {
	var (
		named  []string
		causes []error
	)
	for _, rem := range left {
		r := rem.order
		var (
			state  string
			exists bool
		)
		err := once(ctx, opTimeout, func(ctx context.Context) error {
			var err error
			state, exists, err = c.Observe(ctx, r)
			return err
		})
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			named = append(named, fmt.Sprintf("%s (%s, unreadable)", r.ID, r.Kind))
			causes = append(causes, fmt.Errorf("observing order %s: %w", r.ID, err))
		case !exists:
			gone[r.ID] = true
			continue
		default:
			named = append(named, fmt.Sprintf("%s (%s, %s)", r.ID, r.Kind, state))
		}
		if rem.err != nil {
			causes = append(causes, rem.err)
		}
	}
	if len(named) == 0 {
		return nil
	}
	still := fmt.Errorf("cleanup: %d orders are still present: %s", len(named), strings.Join(named, ", "))
	return errors.Join(append([]error{still}, causes...)...)
}

// verifyCredentials is step 6: for each credential whose order is in gone, it
// waits up to opTimeout for the Secret beside the order to be gone and then
// sends one login with the credential under opTimeout, which Keystone must
// reject. Every credential is checked; the first error is returned and the
// later ones are logged. It stops when ctx ends, which the caller checks.
func verifyCredentials(ctx context.Context, c Cleaner, creds []delivered, gone map[string]bool, opTimeout time.Duration) error {
	var first error
	keep := func(err error) {
		if first == nil {
			first = err
			return
		}
		slog.Warn("credential check after the delete failed", "error", err)
	}
	for _, d := range creds {
		r := d.order
		if !gone[r.ID] {
			continue
		}
		err := once(ctx, opTimeout, func(ctx context.Context) error { return c.WaitSecretGone(ctx, r) })
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			keep(fmt.Errorf("checking that the secret of deleted order %s is gone: %w", r.ID, err))
		}

		err = once(ctx, opTimeout, func(ctx context.Context) error { return c.Reject(ctx, r.Kind, d.cred) })
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			keep(fmt.Errorf("checking that Keystone rejects the credential of deleted order %s: %w", r.ID, err))
		}
	}
	return first
}

// each runs fn for every order through runStage, up to concurrency at once,
// and returns fn's error for each order, in order. Unlike a failing item of
// runStage, one order's error does not cancel the others. The second result
// is ctx's error when ctx ended.
func each(ctx context.Context, orders []resource.Resource, concurrency int, fn func(context.Context, resource.Resource) error) ([]error, error) {
	type outcome struct{ err error }
	res, err := runStage(ctx, orders, concurrency, func(ctx context.Context, r resource.Resource) (outcome, error) {
		return outcome{err: fn(ctx, r)}, nil
	})
	errs := make([]error, len(orders))
	for i, o := range res {
		errs[i] = o.err
	}
	return errs, err
}

// unionOfKind concatenates the orders of kind in the lists, dropping entries
// of another kind or with an empty id and deduplicating by id, so an order
// found both by its label and in the run record is deleted once.
func unionOfKind(kind resource.Kind, lists ...[]resource.Resource) []resource.Resource {
	seen := make(map[string]bool)
	var out []resource.Resource
	for _, list := range lists {
		for _, r := range list {
			if r.Kind != kind || r.ID == "" || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}
