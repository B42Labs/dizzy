package c5c3

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/B42Labs/dizzy/internal/c5c3/plan"
	"github.com/B42Labs/dizzy/internal/resource"
)

// refusalReasons are the condition reasons that end WaitReady at once. The
// operator reports each when the order, its namespace's assignment or its
// ControlPlane must change before the order can turn ready, so a longer wait
// cannot help. Every other reason is waited through: the *Failed and *Error
// reasons, which the operator retries, and UserNotFound, ProjectNotFound and
// NoRoleOnProject, which its cache can report for a moment about an order
// dizzy has just seen ready.
var refusalReasons = map[string]bool{
	"NamespaceNotAssigned":    true,
	"ControlPlaneNotFound":    true,
	"ClusterNameTooLong":      true,
	"ControlPlaneMismatch":    true,
	"ServiceAccountCollision": true,
	"ProjectCollision":        true,
	"ServiceCollision":        true,
	"RoleNotAllowed":          true,
	"CatalogNotAllowed":       true,
	"DuplicateRoleAssignment": true,
	"KeystoneNotPublished":    true,
	"DeliveryRefused":         true,
}

// OrderStatus is what the stages read of an order. A field the order's kind
// does not have, or its status does not carry yet, is its zero value.
type OrderStatus struct {
	// Ready is true when the Ready condition is True and status.observedGeneration
	// equals metadata.generation.
	Ready bool
	// Deleting is true when metadata.deletionTimestamp is set.
	Deleting bool
	// Reason is the reason of the first condition with status False.
	Reason string
	// SpecGeneration is spec.passwordGeneration of a user order or
	// spec.credentialGeneration of an application credential order.
	SpecGeneration int64
	// Generation is status.passwordGeneration of a user order or
	// status.credentialGeneration of an application credential order.
	Generation int64
	// CredentialID is status.credentialID of an application credential order.
	CredentialID string
	// PreviousCredentialID is status.previousCredentialID of an application
	// credential order, set only during the superseded credential's grace
	// period.
	PreviousCredentialID string
	// PreviousCredentialDeleteAt is status.previousCredentialDeleteAt of an
	// application credential order.
	PreviousCredentialDeleteAt time.Time
	// ProjectName and DomainName are status.projectName and status.domainName
	// of a project order.
	ProjectName, DomainName string
	// ServiceName is status.serviceName of a catalog entry order.
	ServiceName string
}

// condition is one entry of an order's status.conditions.
type condition struct {
	Type, Status, Reason, Message string
}

// orderState is an order as one GET read it: the status the stages read and
// the conditions the waits decide on.
type orderState struct {
	status     OrderStatus
	conditions []condition
}

// refusal returns the first condition with status False whose reason is one
// of refusalReasons, and false when there is none.
func (s orderState) refusal() (condition, bool) {
	for _, cd := range s.conditions {
		if cd.Status == string(metav1.ConditionFalse) && refusalReasons[cd.Reason] {
			return cd, true
		}
	}
	return condition{}, false
}

// reports reports whether a condition with status False carries reason.
func (s orderState) reports(reason string) bool {
	return slices.ContainsFunc(s.conditions, func(cd condition) bool {
		return cd.Status == string(metav1.ConditionFalse) && cd.Reason == reason
	})
}

// generationField names the spec and status field that carries the credential
// generation of kind, and is empty for a kind without one.
func generationField(kind resource.Kind) string {
	switch kind {
	case KindUser:
		return "passwordGeneration"
	case KindApplicationCredential:
		return "credentialGeneration"
	default:
		return ""
	}
}

// parseOrder reads the status of an order of kind from u. It returns an error
// for a field of the wrong type and for a previousCredentialDeleteAt that is
// not an RFC 3339 time.
func parseOrder(kind resource.Kind, u *unstructured.Unstructured) (orderState, error) {
	conditions, err := parseConditions(u)
	if err != nil {
		return orderState{}, err
	}
	observed, err := nestedInt64(u, "status", "observedGeneration")
	if err != nil {
		return orderState{}, err
	}

	st := OrderStatus{Deleting: u.GetDeletionTimestamp() != nil}
	reasonSet := false
	for _, cd := range conditions {
		if cd.Type == "Ready" && cd.Status == string(metav1.ConditionTrue) && observed == u.GetGeneration() {
			st.Ready = true
		}
		if cd.Status == string(metav1.ConditionFalse) && !reasonSet {
			st.Reason, reasonSet = cd.Reason, true
		}
	}

	if field := generationField(kind); field != "" {
		if st.SpecGeneration, err = nestedInt64(u, "spec", field); err != nil {
			return orderState{}, err
		}
		if st.Generation, err = nestedInt64(u, "status", field); err != nil {
			return orderState{}, err
		}
	}
	switch kind {
	case KindApplicationCredential:
		if st.CredentialID, err = nestedString(u, "status", "credentialID"); err != nil {
			return orderState{}, err
		}
		if st.PreviousCredentialID, err = nestedString(u, "status", "previousCredentialID"); err != nil {
			return orderState{}, err
		}
		deleteAt, err := nestedString(u, "status", "previousCredentialDeleteAt")
		if err != nil {
			return orderState{}, err
		}
		if deleteAt != "" {
			if st.PreviousCredentialDeleteAt, err = time.Parse(time.RFC3339, deleteAt); err != nil {
				return orderState{}, fmt.Errorf("order %s: status.previousCredentialDeleteAt: %w", u.GetName(), err)
			}
		}
	case KindProject:
		if st.ProjectName, err = nestedString(u, "status", "projectName"); err != nil {
			return orderState{}, err
		}
		if st.DomainName, err = nestedString(u, "status", "domainName"); err != nil {
			return orderState{}, err
		}
	case KindCatalogEntry:
		if st.ServiceName, err = nestedString(u, "status", "serviceName"); err != nil {
			return orderState{}, err
		}
	}
	return orderState{status: st, conditions: conditions}, nil
}

// parseConditions reads status.conditions of u. A missing status or an empty
// list yields no condition; an entry that is not an object is an error.
func parseConditions(u *unstructured.Unstructured) ([]condition, error) {
	raw, _, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("order %s: %w", u.GetName(), err)
	}
	conditions := make([]condition, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("order %s: status.conditions[%d] is not an object", u.GetName(), i)
		}
		str := func(key string) string {
			s, _ := m[key].(string)
			return s
		}
		conditions = append(conditions, condition{
			Type:    str("type"),
			Status:  str("status"),
			Reason:  str("reason"),
			Message: str("message"),
		})
	}
	return conditions, nil
}

// nestedInt64 reads the integer at fields of u, zero when it is absent.
func nestedInt64(u *unstructured.Unstructured, fields ...string) (int64, error) {
	v, _, err := unstructured.NestedInt64(u.Object, fields...)
	if err != nil {
		return 0, fmt.Errorf("order %s: %w", u.GetName(), err)
	}
	return v, nil
}

// nestedString reads the string at fields of u, empty when it is absent.
func nestedString(u *unstructured.Unstructured, fields ...string) (string, error) {
	v, _, err := unstructured.NestedString(u.Object, fields...)
	if err != nil {
		return "", fmt.Errorf("order %s: %w", u.GetName(), err)
	}
	return v, nil
}

// CreateOrder creates the custom resource of o in the client's namespace,
// named dizzyorder-<runid>-<logical> and labelled with the run id and the
// kind, as one create sample of o's kind. Its spec references the ControlPlane
// SetPlan gave, with controlPlaneRef.namespace only when the plan names one,
// and carries what o's kind needs: the referenced user and project orders by
// their Kubernetes names and the role of a role assignment, the service type
// and public endpoint of a catalog entry, and the referenced orders and the
// plan's grace period of an application credential. An unknown kind, and a
// client without a plan, are refused without a request.
func (c *Client) CreateOrder(ctx context.Context, o plan.Order) (resource.Resource, error) {
	kind := resource.Kind(o.Kind)
	k, ok := lookupKind(kind)
	if !ok {
		return resource.Resource{}, fmt.Errorf("order %q has unknown c5c3 order kind %q", o.Name, o.Kind)
	}
	if c.plan == nil {
		return resource.Resource{}, fmt.Errorf("creating order %q: the client has no plan; SetPlan must come first", o.Name)
	}

	name := resourceName(c.runID, o.Name)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiGroup + "/" + apiVersion,
		"kind":       k.k8sKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": c.namespace,
			"labels": map[string]any{
				labelRun:  c.runID,
				labelType: string(kind),
			},
		},
		"spec": c.orderSpec(o),
	}}

	err := c.timed(ctx, kind, "create", func(ctx context.Context) error {
		_, err := c.dyn.Resource(k.gvr()).Namespace(c.namespace).Create(ctx, obj, metav1.CreateOptions{})
		return err
	})
	if err != nil {
		// A create whose answer was lost can still have created the order. Its
		// labels keep it discoverable at cleanup; the id lets an operator find it.
		slog.Warn("create failed; an order with this name may exist in the namespace",
			"id", orderID(c.namespace, name), "error", err)
		return resource.Resource{}, fmt.Errorf("creating %s order %q: %w", kind, o.Name, err)
	}
	r := resource.Resource{Kind: kind, Logical: o.Name, Name: name, ID: orderID(c.namespace, name)}
	slog.Info("created resource", "kind", r.Kind, "logical", r.Logical, "id", r.ID)
	return r, nil
}

// orderSpec builds the spec of o from the client's plan and run id.
func (c *Client) orderSpec(o plan.Order) map[string]any {
	controlPlane := map[string]any{"name": c.plan.ControlPlane.Name}
	if ns := c.plan.ControlPlane.Namespace; ns != "" {
		controlPlane["namespace"] = ns
	}
	ref := func(logical string) map[string]any {
		return map[string]any{"name": resourceName(c.runID, logical)}
	}

	spec := map[string]any{"controlPlaneRef": controlPlane}
	switch resource.Kind(o.Kind) {
	case KindRoleAssignment:
		spec["userRef"] = ref(o.User)
		spec["projectRef"] = ref(o.Project)
		spec["role"] = o.Role
	case KindCatalogEntry:
		spec["serviceType"] = o.ServiceType
		spec["endpoints"] = []any{
			map[string]any{"interface": "public", "url": o.EndpointURL},
		}
	case KindApplicationCredential:
		spec["userRef"] = ref(o.User)
		spec["projectRef"] = ref(o.Project)
		spec["rotation"] = map[string]any{"gracePeriod": c.plan.GracePeriod.String()}
	}
	return spec
}

// getOrder reads the target once and parses it. It records nothing and
// returns the time the GET took, so the caller records the call with the
// error it decides on.
func (c *Client) getOrder(ctx context.Context, t target) (orderState, time.Duration, error) {
	start := time.Now()
	u, err := c.orders(t).Get(ctx, t.name, metav1.GetOptions{})
	d := time.Since(start)
	if err != nil {
		return orderState{}, d, err
	}
	s, err := parseOrder(t.kind, u)
	return s, d, err
}

// Status reads the order r addresses as one get sample of its kind. A 404 is
// returned wrapped, so a caller can tell a gone order with IsNotFound.
func (c *Client) Status(ctx context.Context, r resource.Resource) (OrderStatus, error) {
	t, err := parseTarget(r)
	if err != nil {
		return OrderStatus{}, err
	}
	s, d, err := c.getOrder(ctx, t)
	c.record(ctx, t.kind, "get", d, err)
	if err != nil {
		return OrderStatus{}, fmt.Errorf("reading order %s: %w", r.ID, err)
	}
	return s.status, nil
}

// readPoll reads the target as one poll of a wait for what. A failed GET is
// recorded as a failed get and reported as not read, so the wait polls on,
// except for a 404, which is returned wrapped to end the wait. An order that
// was read is returned with the GET's duration, for the caller to record.
func (c *Client) readPoll(ctx context.Context, t target, what string) (s orderState, d time.Duration, read bool, err error) {
	s, d, err = c.getOrder(ctx, t)
	switch {
	case IsNotFound(err):
		c.record(ctx, t.kind, "get", d, err)
		return orderState{}, d, false, fmt.Errorf("order %s is gone while waiting %s: %w", t.id, what, err)
	case err != nil:
		c.record(ctx, t.kind, "get", d, err)
		return orderState{}, d, false, nil
	}
	return s, d, true, nil
}

// waitOrder polls the target, each poll one get sample of its kind, until
// done reports the wait over or returns the error it ends with. A poll that
// read the order is a successful get whatever done decides. A 404 ends the
// wait with an error wrapping it, and ctx ending ends it with ctx.Err().
func (c *Client) waitOrder(ctx context.Context, t target, what string, done func(orderState) (bool, error)) error {
	return c.poll(ctx, func(ctx context.Context) (bool, error) {
		s, d, read, err := c.readPoll(ctx, t, what)
		if !read {
			return false, err
		}
		c.record(ctx, t.kind, "get", d, nil)
		return done(s)
	})
}

// WaitReady polls the order r addresses until it is ready and records one
// time-to-ready sample of its kind over the whole wait, successful only when
// the order turned ready. It ends at once with ErrRefused, quoting the
// condition's reason and message, when a condition with status False carries
// one of refusalReasons; that poll is recorded as a failed get with error kind
// refused. Every other reason is waited through. A 404 ends the wait with an
// error wrapping it, and the context's end with ctx.Err().
func (c *Client) WaitReady(ctx context.Context, r resource.Resource) error {
	t, err := parseTarget(r)
	if err != nil {
		return err
	}
	start := time.Now()
	err = c.poll(ctx, func(ctx context.Context) (bool, error) {
		s, d, read, err := c.readPoll(ctx, t, "for readiness")
		if !read {
			return false, err
		}
		if cd, refused := s.refusal(); refused {
			err := fmt.Errorf("%w: order %s reports %s on %s: %s", ErrRefused, r.ID, cd.Reason, cd.Type, cd.Message)
			c.record(ctx, t.kind, "get", d, err)
			return false, err
		}
		c.record(ctx, t.kind, "get", d, nil)
		return s.status.Ready, nil
	})
	c.recordReady(ctx, t.kind, time.Since(start), err == nil)
	return err
}

// WaitReason polls the order r addresses until a condition with status False
// carries reason, and records one refuse sample of its kind over the whole
// wait. It returns ErrMismatch when the order turns ready instead, or reports
// one of refusalReasons other than reason. A 404 ends the wait with an error
// wrapping it, and the context's end with ctx.Err().
func (c *Client) WaitReason(ctx context.Context, r resource.Resource, reason string) error {
	t, err := parseTarget(r)
	if err != nil {
		return err
	}
	start := time.Now()
	err = c.waitOrder(ctx, t, "for reason "+reason, func(s orderState) (bool, error) {
		if s.reports(reason) {
			return true, nil
		}
		if cd, refused := s.refusal(); refused {
			return false, fmt.Errorf("%w: order %s reports %s on %s instead of %s: %s",
				ErrMismatch, r.ID, cd.Reason, cd.Type, reason, cd.Message)
		}
		if s.status.Ready {
			return false, fmt.Errorf("%w: order %s turned ready instead of reporting %s", ErrMismatch, r.ID, reason)
		}
		return false, nil
	})
	c.record(ctx, t.kind, "refuse", time.Since(start), err)
	return err
}

// WaitGone polls the order r addresses until it is gone and records one
// finalize sample of its kind over the whole wait. Each poll is one get
// sample; the final 404 is the answer the wait expects and is recorded as a
// successful get. Any other failed GET is polled through. It returns
// ctx.Err() when the context ends first.
func (c *Client) WaitGone(ctx context.Context, r resource.Resource) error {
	t, err := parseTarget(r)
	if err != nil {
		return err
	}
	start := time.Now()
	err = c.poll(ctx, func(ctx context.Context) (bool, error) {
		begin := time.Now()
		_, err := c.orders(t).Get(ctx, t.name, metav1.GetOptions{})
		d := time.Since(begin)
		if IsNotFound(err) {
			c.record(ctx, t.kind, "get", d, nil)
			return true, nil
		}
		c.record(ctx, t.kind, "get", d, err)
		return false, nil
	})
	c.record(ctx, t.kind, "finalize", time.Since(start), err)
	return err
}

// Delete deletes the order r addresses as one delete sample of its kind. A
// 404 is success, which keeps cleanup idempotent.
func (c *Client) Delete(ctx context.Context, r resource.Resource) error {
	t, err := parseTarget(r)
	if err != nil {
		return err
	}
	err = c.timed(ctx, t.kind, "delete", func(ctx context.Context) error {
		err := c.orders(t).Delete(ctx, t.name, metav1.DeleteOptions{})
		if IsNotFound(err) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("deleting order %s: %w", r.ID, err)
	}
	return nil
}

// RaiseGeneration rotates the credential of a user or application credential
// order at once: it reads the order's status generation N through Status,
// merge-patches spec.passwordGeneration or spec.credentialGeneration to N+1 as
// one update sample, and returns N+1. Any other kind is refused without a
// request. A 409 to the patch is retryable.
func (c *Client) RaiseGeneration(ctx context.Context, r resource.Resource) (int64, error) {
	t, err := parseTarget(r)
	if err != nil {
		return 0, err
	}
	field := generationField(t.kind)
	if field == "" {
		return 0, fmt.Errorf("order %s is a %s order, which has no credential generation to raise", r.ID, t.kind)
	}

	st, err := c.Status(ctx, r)
	if err != nil {
		return 0, err
	}
	next := st.Generation + 1
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{field: next}})
	if err != nil {
		return 0, fmt.Errorf("encoding the generation patch of order %s: %w", r.ID, err)
	}
	err = c.timed(ctx, t.kind, "update", func(ctx context.Context) error {
		_, err := c.orders(t).Patch(ctx, t.name, types.MergePatchType, patch, metav1.PatchOptions{})
		return wrapPatchError(err)
	})
	if err != nil {
		return 0, fmt.Errorf("raising spec.%s of order %s to %d: %w", field, r.ID, next, err)
	}
	return next, nil
}

// WaitRotated polls the user or application credential order r addresses
// until it is ready with a status generation of at least generation, then
// reads its delivered Secret and returns the credential. It returns
// ErrMismatch when the Secret still carries previous's password, for a user,
// or previous's application credential id, for a credential order. It records
// one rotate sample of the order's kind over the whole wait. A 404 ends the
// wait with an error wrapping it, and the context's end with ctx.Err(). Any
// other kind is refused without a request.
func (c *Client) WaitRotated(ctx context.Context, r resource.Resource, generation int64, previous Credential) (Credential, error) {
	t, err := parseTarget(r)
	if err != nil {
		return Credential{}, err
	}
	if generationField(t.kind) == "" {
		return Credential{}, fmt.Errorf("order %s is a %s order, which does not rotate", r.ID, t.kind)
	}

	start := time.Now()
	var cred Credential
	err = c.waitOrder(ctx, t, fmt.Sprintf("for generation %d", generation), func(s orderState) (bool, error) {
		return s.status.Ready && s.status.Generation >= generation, nil
	})
	if err == nil {
		cred, err = c.ReadCredential(ctx, r)
	}
	if err == nil {
		err = checkRotated(t, cred, previous, generation)
	}
	c.record(ctx, t.kind, "rotate", time.Since(start), err)
	if err != nil {
		return Credential{}, err
	}
	return cred, nil
}

// checkRotated returns ErrMismatch when cred, read after the order reached
// generation, still carries the password or application credential id of
// previous.
func checkRotated(t target, cred, previous Credential, generation int64) error {
	secret := orderID(t.namespace, secretName(t.name))
	switch {
	case t.kind == KindUser && cred.Password == previous.Password:
		return fmt.Errorf("%w: secret %s still carries the password of before generation %d", ErrMismatch, secret, generation)
	case t.kind == KindApplicationCredential && cred.ApplicationCredentialID == previous.ApplicationCredentialID:
		return fmt.Errorf("%w: secret %s still carries application credential %s of before generation %d",
			ErrMismatch, secret, cred.ApplicationCredentialID, generation)
	}
	return nil
}

// WaitPreviousGone polls the order r addresses until its status names no
// superseded credential any more: the operator deleted it once the grace
// period passed. A 404 ends the wait with an error wrapping it, and the
// context's end with ctx.Err().
func (c *Client) WaitPreviousGone(ctx context.Context, r resource.Resource) error {
	t, err := parseTarget(r)
	if err != nil {
		return err
	}
	return c.waitOrder(ctx, t, "for the superseded credential to go", func(s orderState) (bool, error) {
		return s.status.PreviousCredentialID == "", nil
	})
}

// WaitGenerationSettled polls the order r addresses until its status
// generation has caught up with its spec generation, or the order is being
// deleted. Cleanup waits on it for a user order before it deletes the
// application credentials, whose teardown can strand while the user's
// password rotates. A 404 ends the wait with an error wrapping it, and the
// context's end with ctx.Err().
func (c *Client) WaitGenerationSettled(ctx context.Context, r resource.Resource) error {
	t, err := parseTarget(r)
	if err != nil {
		return err
	}
	return c.waitOrder(ctx, t, "for the generation to settle", func(s orderState) (bool, error) {
		return s.status.Generation >= s.status.SpecGeneration || s.status.Deleting, nil
	})
}

// ListByRun returns the orders of kind in the client's namespace that carry
// the label dizzy/run=<runID>, the discovery step cleanup deletes from. It is
// one list sample of kind. An unknown kind is refused without a request.
func (c *Client) ListByRun(ctx context.Context, kind resource.Kind, runID string) ([]resource.Resource, error) {
	found, err := c.list(ctx, kind, labelRun+"="+runID)
	if err != nil {
		return nil, fmt.Errorf("listing %s orders of run %s: %w", kind, runID, err)
	}
	return found, nil
}

// ListByAnyRun returns every order of kind in the client's namespace that
// carries a dizzy/type label, whatever its run: the pre-flight sweep's handle
// for reclaiming a killed iteration's orders once the run id is lost. It is
// one list sample of kind. An unknown kind is refused without a request.
func (c *Client) ListByAnyRun(ctx context.Context, kind resource.Kind) ([]resource.Resource, error) {
	found, err := c.list(ctx, kind, labelType)
	if err != nil {
		return nil, fmt.Errorf("listing %s orders of any run: %w", kind, err)
	}
	return found, nil
}

// list returns the orders of kind in the client's namespace that selector
// matches, sorted by name, as one list sample of kind. Each order's logical
// name is its name without the prefix of the run its dizzy/run label names,
// and empty when the name does not carry that prefix.
func (c *Client) list(ctx context.Context, kind resource.Kind, selector string) ([]resource.Resource, error) {
	k, ok := lookupKind(kind)
	if !ok {
		return nil, fmt.Errorf("unknown c5c3 order kind %q", kind)
	}
	var found []resource.Resource
	err := c.timed(ctx, kind, "list", func(ctx context.Context) error {
		list, err := c.dyn.Resource(k.gvr()).Namespace(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}
		found = nil
		for _, item := range list.Items {
			name := item.GetName()
			logical, ok := strings.CutPrefix(name, runPrefix(item.GetLabels()[labelRun]))
			if !ok {
				logical = ""
			}
			found = append(found, resource.Resource{
				Kind:    kind,
				Logical: logical,
				Name:    name,
				ID:      orderID(item.GetNamespace(), name),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(found, func(a, b resource.Resource) int { return strings.Compare(a.Name, b.Name) })
	return found, nil
}

// Observe re-reads the order r addresses as one get sample of its kind and
// returns its state for the status table: Ready for a ready order, otherwise
// the reason of its first condition with status False, otherwise Pending. A
// 404 is reported as ("", false, nil), so an order deleted out of band reads
// as gone rather than as a failure. A malformed id and an unknown kind are
// refused without a request.
func (c *Client) Observe(ctx context.Context, r resource.Resource) (status string, exists bool, err error) {
	t, err := parseTarget(r)
	if err != nil {
		return "", false, err
	}
	s, d, err := c.getOrder(ctx, t)
	c.record(ctx, t.kind, "get", d, err)
	switch {
	case IsNotFound(err):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("reading order %s: %w", r.ID, err)
	case s.status.Ready:
		return "Ready", true, nil
	case s.status.Reason != "":
		return s.status.Reason, true, nil
	default:
		return "Pending", true, nil
	}
}
