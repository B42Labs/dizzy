package nova

import (
	"context"
	"errors"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

// respErr builds a gophercloud unexpected-response error for the given status
// and body, the shape the classifiers branch on.
func respErr(status int, body string) error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: status, Body: []byte(body)}
}

func TestErrKind(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"quota 413", respErr(413, "over limit"), "quota"},
		{"quota 403 body", respErr(403, "Quota exceeded for instances"), "quota"},
		{"quota 409 neutron body", respErr(409, "Quota exceeded for resources: ['port']"), "quota"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"http 404", respErr(404, "not found"), "http_404"},
		{"http 409 non-quota", respErr(409, "conflict"), "http_409"},
		{"http 500", respErr(500, "boom"), "http_500"},
		{"http other", respErr(418, "teapot"), "http_other"},
		{"other", errors.New("dial tcp: connection refused"), "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := errKind(tc.err); got != tc.want {
				t.Errorf("errKind(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsQuota(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"413 over limit", respErr(413, "anything"), true},
		{"403 with quota body", respErr(403, "Quota exceeded for cores"), true},
		{"403 without quota body", respErr(403, "policy forbids this"), false},
		{"400 with quota body", respErr(400, "over quota"), true},
		// Neutron signals over-quota for the companion networks/subnets/ports with
		// a 409 whose body names the quota; a bare 409 conflict is not a quota.
		{"409 with quota body", respErr(409, "Quota exceeded for resources: ['port']"), true},
		{"409 without quota body", respErr(409, "instance is in task_state resize_prep"), false},
		{"404", respErr(404, "quota"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isQuota(tc.err); got != tc.want {
				t.Errorf("isQuota(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"quota 413", respErr(413, "over limit"), false},
		{"500", respErr(500, "boom"), true},
		{"503", respErr(503, "unavailable"), true},
		{"429", respErr(429, "slow down"), true},
		{"409 non-quota", respErr(409, "conflict"), true},
		// A Neutron over-quota 409 fast-fails rather than burning retries.
		{"409 quota", respErr(409, "Quota exceeded for resources: ['port']"), false},
		{"404", respErr(404, "gone"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(respErr(404, "gone")) {
		t.Error("IsNotFound(404) = false, want true")
	}
	if IsNotFound(respErr(409, "conflict")) {
		t.Error("IsNotFound(409) = true, want false")
	}
	if IsNotFound(nil) {
		t.Error("IsNotFound(nil) = true, want false")
	}
}

func TestConflictMentions(t *testing.T) {
	// Nova rejects stopping an already-stopped instance with a 409 whose body
	// names the current vm_state; a retried stop that hits it is a success.
	stopped := respErr(409, "Cannot 'stop' instance while it is in vm_state stopped")
	if !conflictMentions(stopped, "stopped") {
		t.Error("conflictMentions(409 stopped, \"stopped\") = false, want true")
	}
	// A 409 that does not name the target state is a real conflict.
	if conflictMentions(respErr(409, "instance is locked"), "stopped") {
		t.Error("conflictMentions(409 locked, \"stopped\") = true, want false")
	}
	// A non-409 never matches.
	if conflictMentions(respErr(400, "vm_state stopped"), "stopped") {
		t.Error("conflictMentions(400, \"stopped\") = true, want false")
	}
}

func TestResourceNameAndIdentity(t *testing.T) {
	if got, want := resourceName("abc123", "srv-0001"), "dizzy-abc123-srv-0001"; got != want {
		t.Errorf("resourceName = %q, want %q", got, want)
	}
	md := runMetadata("abc123", KindServer)
	if md[metaRun] != "abc123" || md[metaType] != string(KindServer) {
		t.Errorf("runMetadata = %v, want run=abc123 type=server", md)
	}
	tags := runTags("abc123", KindNetwork)
	if len(tags) != 2 || tags[0] != "dizzy:run=abc123" || tags[1] != "dizzy:type=network" {
		t.Errorf("runTags = %v, want [dizzy:run=abc123 dizzy:type=network]", tags)
	}
}

func TestReadyAndTerminalStates(t *testing.T) {
	if !readyState(KindServer, statusServerActive) {
		t.Error("server ACTIVE should be ready")
	}
	if readyState(KindServer, statusServerShutoff) {
		t.Error("server SHUTOFF should not be ready")
	}
	if !readyState(KindPort, "DOWN") {
		t.Error("port DOWN should be ready")
	}
	if !readyState(KindVolume, statusVolumeAvailable) {
		t.Error("volume available should be ready")
	}
	if !terminalState(KindServer, statusServerError) {
		t.Error("server ERROR should be terminal")
	}
	if !terminalState(KindVolume, "error_deleting") {
		t.Error("volume error_deleting should be terminal")
	}
	if terminalState(KindNetwork, "DOWN") {
		t.Error("network has no terminal state")
	}
}

func TestMetadataMatches(t *testing.T) {
	md := map[string]string{"dizzy:run": "r1", "dizzy:type": "server"}
	if !metadataMatches(md, map[string]string{"dizzy:run": "r1"}) {
		t.Error("metadataMatches should match a subset")
	}
	if metadataMatches(md, map[string]string{"dizzy:run": "r2"}) {
		t.Error("metadataMatches should reject a mismatched value")
	}
}
