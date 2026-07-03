package keystone

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the warnings the client emits on create/list failures so the
// package's test output stays readable; the failure-path tests assert on the
// returned error, not the log line.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
