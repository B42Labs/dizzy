// Package scenarios embeds the built-in scenario profiles shipped under
// scenarios/ so callers can read them without depending on the process working
// directory.
package scenarios

import "embed"

// Files holds the built-in scenario profile YAML files, addressed by their
// service-scoped path (for example "neutron/medium.yaml", "cinder/small.yaml",
// "keystone/small.yaml", "nova/small.yaml", "glance/small.yaml",
// "barbican/small.yaml", "octavia/small.yaml", "mix/small.yaml",
// "c5c3/small.yaml", or "horizon/small.yaml").
//
//go:embed neutron cinder keystone nova glance barbican octavia mix c5c3 horizon
var Files embed.FS
