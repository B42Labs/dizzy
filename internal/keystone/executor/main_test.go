package executor

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the per-operation progress logs the executor emits so the
// package's test output stays readable. Tests that assert on a specific log line
// install their own handler locally.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
