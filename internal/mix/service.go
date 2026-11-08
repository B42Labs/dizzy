package mix

import (
	"fmt"
	"slices"
	"strings"

	mixplan "github.com/B42Labs/dizzy/internal/mix/plan"
)

// SupportedServices returns the opt-in services this build supports, in a new
// slice the caller may change. A scenario's services key and a run record's
// services list may name only these. The binding of each supported service to
// the persona whose lane it extends lives in cmd/dizzy/mix_octavia.go
// (mixServices), not here: a binder needs the command's options, its cloud,
// --timeout and --concurrency, which this package cannot see.
func SupportedServices() []string {
	return []string{mixplan.ServiceOctavia}
}

// CheckServices returns an error naming the first entry of names that this
// build does not support as an opt-in service, with the services it does
// support, and nil when it supports every entry. A nil or empty list names no
// service and is always accepted. The commands call it before any API call,
// so a scenario or record naming a service this build cannot bind fails
// before anything is created or deleted.
func CheckServices(names []string) error {
	supported := SupportedServices()
	for _, name := range names {
		if !slices.Contains(supported, name) {
			return fmt.Errorf("opt-in service %q is not supported by this build of dizzy (supported: %s)", name, strings.Join(supported, ", "))
		}
	}
	return nil
}
