// Package plan defines the orders one c5c3 run places with the c5c3-operator
// and the refusals it expects: the expected-state source of truth produced
// from a scenario. Like the other services' plans it is pure data, every
// collection a slice, so encoding a plan to JSON yields byte-identical output
// for the same scenario. A c5c3 plan has no seed: its shape is fixed, and only
// the scenario's values fill it.
package plan

import (
	"fmt"
	"strings"
	"time"
)

// The order kinds, in the snake case dizzy uses for metric labels. Each names
// one c5c3.io/v1alpha1 kind: KeystoneUser, KeystoneProject,
// KeystoneRoleAssignment, KeystoneCatalogEntry and
// KeystoneApplicationCredential.
const (
	KindUser                  = "keystone_user"
	KindProject               = "keystone_project"
	KindRoleAssignment        = "keystone_role_assignment"
	KindCatalogEntry          = "keystone_catalog_entry"
	KindApplicationCredential = "keystone_application_credential"
)

// Plan is everything one c5c3 run orders. Orders are created and must turn
// Ready; Refusals are created and must report their Reason instead. The
// durations bound the waits for the operator, not single requests.
type Plan struct {
	Scenario     string        `json:"scenario"`
	Namespace    string        `json:"namespace"`
	ControlPlane ControlPlane  `json:"controlPlane"`
	Orders       []Order       `json:"orders"`
	Refusals     []Refusal     `json:"refusals"`
	GracePeriod  time.Duration `json:"gracePeriod"`
	ReadyTimeout time.Duration `json:"readyTimeout"`
	GoneTimeout  time.Duration `json:"goneTimeout"`
	SecretRepair bool          `json:"secretRepair"`
}

// ControlPlane is the ControlPlane every order references. An empty Namespace
// means the orders' own namespace.
type ControlPlane struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// Order is one custom resource dizzy creates. Name is its logical name; User
// and Project name, by logical name, the user and project orders a role
// assignment or application credential references. Role is the assigned role,
// and ServiceType and EndpointURL describe a catalog entry.
type Order struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	User        string `json:"user,omitempty"`
	Project     string `json:"project,omitempty"`
	Role        string `json:"role,omitempty"`
	ServiceType string `json:"serviceType,omitempty"`
	EndpointURL string `json:"endpointURL,omitempty"`
}

// Refusal is an order the operator must refuse, with the condition reason it
// must report.
type Refusal struct {
	Order  Order  `json:"order"`
	Reason string `json:"reason"`
}

// Validate checks that every order and refusal has a known kind and a
// non-empty logical name unique across both slices, that every role
// assignment and application credential names its user and project, each an
// order in Orders of the right kind, that every role assignment names a role,
// and that every catalog entry names a service type and an endpoint URL. It
// returns the first problem it finds.
func (p *Plan) Validate() error {
	orders := make(map[string]string, len(p.Orders))
	seen := make(map[string]bool, len(p.Orders)+len(p.Refusals))
	all := make([]Order, 0, len(p.Orders)+len(p.Refusals))
	for _, o := range p.Orders {
		orders[o.Name] = o.Kind
		all = append(all, o)
	}
	for _, r := range p.Refusals {
		if r.Reason == "" {
			return fmt.Errorf("refusal %q names no reason", r.Order.Name)
		}
		all = append(all, r.Order)
	}

	for _, o := range all {
		if o.Name == "" {
			return fmt.Errorf("order of kind %q has an empty name", o.Kind)
		}
		if seen[o.Name] {
			return fmt.Errorf("duplicate order name %q", o.Name)
		}
		seen[o.Name] = true

		switch o.Kind {
		case KindUser, KindProject:
		case KindRoleAssignment, KindApplicationCredential:
			if err := checkRef(o, "user", o.User, KindUser, orders); err != nil {
				return err
			}
			if err := checkRef(o, "project", o.Project, KindProject, orders); err != nil {
				return err
			}
			if o.Kind == KindRoleAssignment && o.Role == "" {
				return fmt.Errorf("order %q names no role", o.Name)
			}
		case KindCatalogEntry:
			if o.ServiceType == "" || o.EndpointURL == "" {
				return fmt.Errorf("order %q needs a service type and an endpoint URL", o.Name)
			}
		default:
			return fmt.Errorf("order %q has unknown kind %q", o.Name, o.Kind)
		}
	}
	return nil
}

// checkRef reports whether ref, the field of order o, names an order of kind
// want in orders.
func checkRef(o Order, field, ref, want string, orders map[string]string) error {
	if ref == "" {
		return fmt.Errorf("order %q names no %s", o.Name, field)
	}
	kind, ok := orders[ref]
	if !ok {
		return fmt.Errorf("order %q references unknown %s order %q", o.Name, field, ref)
	}
	if kind != want {
		return fmt.Errorf("order %q references %q as its %s, which is a %s order", o.Name, ref, field, kind)
	}
	return nil
}

// Summary returns the human-readable plan "c5c3 apply --dry-run" prints: the
// namespace, the ControlPlane, one line per order and refusal, the grace
// period, both timeouts and whether the Secret repair check runs.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q\n", p.Scenario)
	fmt.Fprintf(&b, "  namespace:      %s\n", p.Namespace)
	cpNamespace := p.ControlPlane.Namespace
	if cpNamespace == "" {
		cpNamespace = p.Namespace
	}
	fmt.Fprintf(&b, "  control plane:  %s/%s\n", cpNamespace, p.ControlPlane.Name)
	for _, o := range p.Orders {
		fmt.Fprintf(&b, "  order:          %s\n", describe(o))
	}
	for _, r := range p.Refusals {
		fmt.Fprintf(&b, "  refusal:        %s, expects %s\n", describe(r.Order), r.Reason)
	}
	fmt.Fprintf(&b, "  grace period:   %s\n", p.GracePeriod)
	fmt.Fprintf(&b, "  ready timeout:  %s\n", p.ReadyTimeout)
	fmt.Fprintf(&b, "  gone timeout:   %s\n", p.GoneTimeout)
	repair := "off"
	if p.SecretRepair {
		repair = "on"
	}
	fmt.Fprintf(&b, "  secret repair:  %s\n", repair)
	return b.String()
}

// describe renders one order as its logical name, its kind and the fields its
// kind carries.
func describe(o Order) string {
	s := o.Name + " (" + o.Kind + ")"
	if o.User != "" {
		s += " user=" + o.User
	}
	if o.Project != "" {
		s += " project=" + o.Project
	}
	if o.Role != "" {
		s += " role=" + o.Role
	}
	if o.ServiceType != "" {
		s += " service_type=" + o.ServiceType
	}
	if o.EndpointURL != "" {
		s += " endpoint_url=" + o.EndpointURL
	}
	return s
}
