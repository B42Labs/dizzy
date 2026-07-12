package novagraph

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the per-action churn progress logs the engine emits so the
// package's test output stays readable across the many-step churn runs the tests
// drive.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
