// Package resource holds the service-agnostic cloud identity of a created
// resource. It is shared by the per-service client packages (neutron, cinder)
// and by the run record, so a run's created list has one shape regardless of
// which OpenStack service produced it.
package resource

// Kind names a resource type. It doubles as the metrics "type" label and the
// tag/metadata value written under ostester:type.
type Kind string

// Resource is the cloud identity of a created resource. Logical is the plan's
// reference name (e.g. "net-0001" or "vol-0001"); Name is the applied cloud
// name; ID is the service's UUID. An executor collects these and a later
// cleanup consumes them.
type Resource struct {
	Kind    Kind   `json:"kind"`
	Logical string `json:"logical"`
	Name    string `json:"name"`
	ID      string `json:"id"`
}
