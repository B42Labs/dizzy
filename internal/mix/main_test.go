package mix

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the per-action churn logs the engines emit so the
// package's test output stays readable.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
