// Command dizzy is a scenario-driven load and consistency tester for
// OpenStack, starting with Neutron (networking).
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
