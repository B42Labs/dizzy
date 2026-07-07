// Command dizzy is a scenario-driven load and consistency tester for
// OpenStack, starting with Neutron (networking).
package main

import (
	"context"
	"fmt"
	"os"
)

// version is the build version reported by "dizzy --version". Release builds
// inject the tag via -ldflags "-X main.version=<tag>" (see
// .github/workflows/release.yml); non-release binaries report "dev".
var version = "dev"

func main() {
	if err := newRootCmd().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
