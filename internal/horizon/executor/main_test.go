package executor

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the executor's progress logs so the package's test
// output stays readable.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
