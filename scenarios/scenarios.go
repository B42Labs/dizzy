// Package scenarios embeds the built-in scenario profiles shipped under
// scenarios/ so callers can read them without depending on the process working
// directory.
package scenarios

import "embed"

// Files holds the built-in scenario profile YAML files, addressed by their
// service-scoped path (for example "neutron/medium.yaml", "cinder/small.yaml",
// "keystone/small.yaml", or "nova/small.yaml").
//
//go:embed neutron cinder keystone nova
var Files embed.FS
