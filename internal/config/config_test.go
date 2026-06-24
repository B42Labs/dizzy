package config

import (
	"strings"
	"testing"
)

// TestNewNetworkClient_Errors exercises the failure paths that do not require a
// reachable cloud: an unresolvable cloud name and a missing clouds.yaml. Both
// must surface a wrapped error rather than a nil client.
func TestNewNetworkClient_Errors(t *testing.T) {
	tests := []struct {
		name        string
		cloudName   string
		osCloud     string
		configFile  string
		wantErrPart string
	}{
		{
			name:        "empty cloud name and no OS_CLOUD",
			cloudName:   "",
			osCloud:     "",
			wantErrPart: "parsing clouds.yaml",
		},
		{
			name:        "clouds.yaml file not found",
			cloudName:   "does-not-exist",
			configFile:  "/nonexistent/path/clouds.yaml",
			wantErrPart: "parsing clouds.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OS_CLOUD", tc.osCloud)
			t.Setenv("OS_CLIENT_CONFIG_FILE", tc.configFile)

			client, err := NewNetworkClient(t.Context(), tc.cloudName)
			if err == nil {
				t.Fatalf("expected an error, got nil (client=%v)", client)
			}
			if client != nil {
				t.Errorf("expected nil client on error, got %v", client)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrPart)
			}
		})
	}
}
