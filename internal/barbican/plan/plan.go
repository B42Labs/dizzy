// Package plan defines the expanded, fully-enumerated set of Barbican resources
// (stored secrets, key orders, generic containers, secret ACLs, and container
// consumers): the expected-state source of truth produced deterministically
// from a scenario plus a seed. Like the other services' plans it is pure data:
// every collection is a slice (never a map) so that encoding the plan to JSON
// yields byte-identical output for the same input. A stored secret's payload is not part of the plan: its bytes are
// derived from the plan seed and the secret's logical name, so the plan records
// only the payload size, and a read-back can compare against the same bytes.
package plan

import (
	"fmt"
	"strings"
)

// Plan is the fully-expanded expected state for one Barbican run. Scenario and
// Seed record the provenance that produced it; the slices enumerate every
// stored secret, key order, container, secret ACL, and container consumer. A
// container and an ACL reference stored secrets, and a consumer a container, by
// logical name, resolved by Validate. ACLs and Consumers are omitted from the
// JSON when empty, so a plan without them encodes as it did before they existed.
type Plan struct {
	Scenario   string      `json:"scenario"`
	Seed       int64       `json:"seed"`
	Secrets    []Secret    `json:"secrets"`
	Orders     []Order     `json:"orders"`
	Containers []Container `json:"containers"`
	ACLs       []ACL       `json:"acls,omitempty"`
	Consumers  []Consumer  `json:"consumers,omitempty"`
}

// Secret is one opaque secret dizzy stores with a synthetic payload.
// PayloadBytes is the size of that payload in bytes. Retrieve schedules a
// read-back of the payload, byte-compared with the expected bytes.
type Secret struct {
	Name         string `json:"name"`
	PayloadBytes int    `json:"payloadBytes"`
	Retrieve     bool   `json:"retrieve,omitempty"`
}

// Order is one asynchronous key order dizzy submits. Barbican generates the key
// server side, so the order carries no payload; once ACTIVE it names the
// generated secret, which shares the order's logical name.
type Order struct {
	Name string `json:"name"`
}

// Container is one generic container grouping stored secrets. Secrets lists the
// logical names of the stored secrets it references, in reference order. An
// order's generated secret is never referenced.
type Container struct {
	Name    string   `json:"name"`
	Secrets []string `json:"secrets"`
}

// ACL is the read ACL dizzy sets on one stored secret. Secret is the stored
// secret's logical name, and Users the user ids the ACL lists, in the order the
// generator drew them. The ids are synthetic: Barbican accepts any string as a
// user id without asking Keystone, so a run needs no user beyond its own.
type ACL struct {
	Name   string   `json:"name"`
	Secret string   `json:"secret"`
	Users  []string `json:"users"`
}

// Consumer is one consumer dizzy registers on a container, named by the
// container's logical name. Its cloud name and URL derive from Name, so the
// plan records neither.
type Consumer struct {
	Name      string `json:"name"`
	Container string `json:"container"`
}

// Validate checks the plan for well-formedness: every secret, order, container,
// ACL, and consumer has a non-empty name unique within its collection, every
// secret has a payload of at least one byte, and every container reference is
// non-empty, names a stored secret (an order's name does not count), and
// appears once in its container. Every ACL names a stored secret no other ACL
// names and lists at least one user, each non-empty and listed once, and every
// consumer names a container. Empty collections are valid. It returns an error
// naming the first offender.
func (p *Plan) Validate() error {
	secrets := make(map[string]bool, len(p.Secrets))
	for _, s := range p.Secrets {
		if s.Name == "" {
			return fmt.Errorf("secret has an empty name")
		}
		if secrets[s.Name] {
			return fmt.Errorf("duplicate secret name %q", s.Name)
		}
		secrets[s.Name] = true
		if s.PayloadBytes < 1 {
			return fmt.Errorf("secret %q has payload size %d bytes, want at least 1", s.Name, s.PayloadBytes)
		}
	}

	orders := make(map[string]bool, len(p.Orders))
	for _, o := range p.Orders {
		if o.Name == "" {
			return fmt.Errorf("order has an empty name")
		}
		if orders[o.Name] {
			return fmt.Errorf("duplicate order name %q", o.Name)
		}
		orders[o.Name] = true
	}

	containers := make(map[string]bool, len(p.Containers))
	for _, ct := range p.Containers {
		if ct.Name == "" {
			return fmt.Errorf("container has an empty name")
		}
		if containers[ct.Name] {
			return fmt.Errorf("duplicate container name %q", ct.Name)
		}
		containers[ct.Name] = true
		refs := make(map[string]bool, len(ct.Secrets))
		for _, ref := range ct.Secrets {
			if ref == "" {
				return fmt.Errorf("container %q has an empty secret reference", ct.Name)
			}
			if !secrets[ref] {
				return fmt.Errorf("container %q references unknown secret %q", ct.Name, ref)
			}
			if refs[ref] {
				return fmt.Errorf("container %q references secret %q more than once", ct.Name, ref)
			}
			refs[ref] = true
		}
	}

	acls := make(map[string]bool, len(p.ACLs))
	secured := make(map[string]bool, len(p.ACLs))
	for _, a := range p.ACLs {
		if a.Name == "" {
			return fmt.Errorf("acl has an empty name")
		}
		if acls[a.Name] {
			return fmt.Errorf("duplicate acl name %q", a.Name)
		}
		acls[a.Name] = true
		if !secrets[a.Secret] {
			return fmt.Errorf("acl %q references unknown secret %q", a.Name, a.Secret)
		}
		if secured[a.Secret] {
			return fmt.Errorf("secret %q carries more than one acl", a.Secret)
		}
		secured[a.Secret] = true
		if len(a.Users) == 0 {
			return fmt.Errorf("acl %q lists no users", a.Name)
		}
		users := make(map[string]bool, len(a.Users))
		for _, u := range a.Users {
			if u == "" {
				return fmt.Errorf("acl %q has an empty user", a.Name)
			}
			if users[u] {
				return fmt.Errorf("acl %q lists user %q more than once", a.Name, u)
			}
			users[u] = true
		}
	}

	consumers := make(map[string]bool, len(p.Consumers))
	for _, con := range p.Consumers {
		if con.Name == "" {
			return fmt.Errorf("consumer has an empty name")
		}
		if consumers[con.Name] {
			return fmt.Errorf("duplicate consumer name %q", con.Name)
		}
		consumers[con.Name] = true
		if !containers[con.Container] {
			return fmt.Errorf("consumer %q references unknown container %q", con.Name, con.Container)
		}
	}

	return nil
}

// TotalPayloadBytes sums the payload size across every stored secret, the byte
// volume the run stores in the service.
func (p *Plan) TotalPayloadBytes() int {
	var total int
	for _, s := range p.Secrets {
		total += s.PayloadBytes
	}
	return total
}

// Retrieves counts the stored secrets the plan reads back.
func (p *Plan) Retrieves() int {
	var n int
	for _, s := range p.Secrets {
		if s.Retrieve {
			n++
		}
	}
	return n
}

// ContainerRefs sums the secret references across every container.
func (p *Plan) ContainerRefs() int {
	var n int
	for _, ct := range p.Containers {
		n += len(ct.Secrets)
	}
	return n
}

// ACLUsers sums the users listed across every ACL.
func (p *Plan) ACLUsers() int {
	var n int
	for _, a := range p.ACLs {
		n += len(a.Users)
	}
	return n
}

// Summary returns a deterministic, human-readable count of the plan, used by
// "barbican apply --dry-run" to preview a scenario without touching a cloud.
// The acls and consumers lines appear only when the plan has any, so a plan
// without them reads as it did before they existed.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q (seed %d)\n", p.Scenario, p.Seed)
	fmt.Fprintf(&b, "  secrets:         %d\n", len(p.Secrets))
	fmt.Fprintf(&b, "  payload total:   %d bytes\n", p.TotalPayloadBytes())
	fmt.Fprintf(&b, "  read-backs:      %d\n", p.Retrieves())
	fmt.Fprintf(&b, "  orders:          %d\n", len(p.Orders))
	fmt.Fprintf(&b, "  containers:      %d (%d secret references)\n", len(p.Containers), p.ContainerRefs())
	if len(p.ACLs) > 0 {
		fmt.Fprintf(&b, "  acls:            %d (%d users)\n", len(p.ACLs), p.ACLUsers())
	}
	if len(p.Consumers) > 0 {
		fmt.Fprintf(&b, "  consumers:       %d\n", len(p.Consumers))
	}
	return b.String()
}
