// Package plan defines the expanded, fully-enumerated set of Barbican resources
// (stored secrets, key orders, and generic containers): the expected-state
// source of truth produced deterministically from a scenario plus a seed. Like
// the other services' plans it is pure data: every collection is a slice (never
// a map) so that encoding the plan to JSON yields byte-identical output for the
// same input. A stored secret's payload is not part of the plan: its bytes are
// derived from the plan seed and the secret's logical name, so the plan records
// only the payload size, and a read-back can compare against the same bytes.
package plan

import (
	"fmt"
	"strings"
)

// Plan is the fully-expanded expected state for one Barbican run. Scenario and
// Seed record the provenance that produced it; the slices enumerate every
// stored secret, key order, and container. A container references stored
// secrets by logical name, resolved by Validate.
type Plan struct {
	Scenario   string      `json:"scenario"`
	Seed       int64       `json:"seed"`
	Secrets    []Secret    `json:"secrets"`
	Orders     []Order     `json:"orders"`
	Containers []Container `json:"containers"`
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

// Validate checks the plan for well-formedness: every secret, order, and
// container has a non-empty name unique within its collection, every secret
// has a payload of at least one byte, and every container reference is
// non-empty, names a stored secret (an order's name does not count), and
// appears once in its container. Empty collections are valid. It returns an
// error naming the first offender.
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

// Summary returns a deterministic, human-readable count of the plan, used by
// "barbican apply --dry-run" to preview a scenario without touching a cloud.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan for scenario %q (seed %d)\n", p.Scenario, p.Seed)
	fmt.Fprintf(&b, "  secrets:         %d\n", len(p.Secrets))
	fmt.Fprintf(&b, "  payload total:   %d bytes\n", p.TotalPayloadBytes())
	fmt.Fprintf(&b, "  read-backs:      %d\n", p.Retrieves())
	fmt.Fprintf(&b, "  orders:          %d\n", len(p.Orders))
	fmt.Fprintf(&b, "  containers:      %d (%d secret references)\n", len(p.Containers), p.ContainerRefs())
	return b.String()
}
