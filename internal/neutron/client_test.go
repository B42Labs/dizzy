package neutron

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

// respErr builds a gophercloud unexpected-response error for the given status
// and body, the shape the classifiers branch on.
func respErr(status int, body string) error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: status, Body: []byte(body)}
}

// TestResourceNameAndTags checks that names and tags are deterministic and that
// names stay collision-free across kinds and indices.
func TestResourceNameAndTags(t *testing.T) {
	if got := resourceName("abcd1234", "net-0001"); got != "dizzy-abcd1234-net-0001" {
		t.Errorf("resourceName = %q", got)
	}

	tags := runTags("abcd1234", KindNetwork)
	want := []string{"dizzy:run=abcd1234", "dizzy:type=network"}
	if len(tags) != len(want) {
		t.Fatalf("runTags = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Errorf("runTags[%d] = %q, want %q", i, tags[i], want[i])
		}
	}

	// Distinct (kind, index) inputs must produce distinct names.
	seen := make(map[string]string)
	kinds := []Kind{KindNetwork, KindSubnet, KindRouter, KindPort, KindSecurityGroup}
	for _, k := range kinds {
		for i := 1; i <= 3; i++ {
			logical := fmt.Sprintf("%s-%04d", k, i)
			name := resourceName("run0", logical)
			if prev, ok := seen[name]; ok {
				t.Errorf("name collision: %q from %q and %q", name, prev, logical)
			}
			seen[name] = logical
		}
	}
}

// TestTagCollection maps every taggable kind to its Neutron collection path and
// confirms the non-taggable kinds report an empty path.
func TestTagCollection(t *testing.T) {
	cases := map[Kind]string{
		KindNetwork:           "networks",
		KindSubnet:            "subnets",
		KindSubnetPool:        "subnetpools",
		KindRouter:            "routers",
		KindPort:              "ports",
		KindSecurityGroup:     "security-groups",
		KindAddressScope:      "address-scopes",
		KindRouterInterface:   "",
		KindSecurityGroupRule: "",
	}
	for kind, want := range cases {
		if got := tagCollection(kind); got != want {
			t.Errorf("tagCollection(%s) = %q, want %q", kind, got, want)
		}
	}
}

// TestExpectedReady covers the readiness predicate per kind, including the
// port-specific DOWN state and the non-status kinds that are always ready.
func TestExpectedReady(t *testing.T) {
	cases := []struct {
		kind   Kind
		status string
		want   bool
	}{
		{KindNetwork, "ACTIVE", true},
		{KindNetwork, "BUILD", false},
		{KindRouter, "ACTIVE", true},
		{KindRouter, "DOWN", false},
		{KindPort, "ACTIVE", true},
		{KindPort, "DOWN", true},
		{KindPort, "BUILD", false},
	}
	for _, tc := range cases {
		if got := expectedReady(tc.kind, tc.status); got != tc.want {
			t.Errorf("expectedReady(%s, %q) = %v, want %v", tc.kind, tc.status, got, tc.want)
		}
	}

	if isStatusKind(KindSubnet) {
		t.Error("subnet should not be a status kind")
	}
	if !isStatusKind(KindPort) {
		t.Error("port should be a status kind")
	}
}

// TestClassification pins the retry and quota classification that drives the
// executor's retry/fail-fast decisions, plus the metrics error-kind labels.
func TestClassification(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		retryable   bool
		quota       bool
		wantErrKind string
		wantStatus  int
	}{
		{"nil", nil, false, false, "", 0},
		{"500", respErr(500, ""), true, false, "http_500", 500},
		{"503", respErr(503, ""), true, false, "http_503", 503},
		{"429", respErr(429, ""), true, false, "http_429", 429},
		{"409 conflict", respErr(409, "in use"), true, false, "http_409", 409},
		{"409 quota", respErr(409, `{"NeutronError":{"type":"OverQuota"}}`), false, true, "quota", 409},
		{"403 quota", respErr(403, "Quota exceeded for resources"), false, true, "quota", 403},
		{"403 mentions quota but is not over-quota", respErr(403, "Not authorized to view quota"), false, false, "http_403", 403},
		{"409 mentions quota but is not over-quota", respErr(409, "conflict with quota object"), true, false, "http_409", 409},
		{"404", respErr(404, ""), false, false, "http_404", 404},
		{"418 unlisted code collapses to http_other", respErr(418, ""), false, false, "http_other", 418},
		{"timeout", context.DeadlineExceeded, false, false, "timeout", 0},
		{"canceled", context.Canceled, false, false, "canceled", 0},
		{"transport", errors.New("connection refused"), false, false, "other", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.retryable {
				t.Errorf("IsRetryable = %v, want %v", got, tc.retryable)
			}
			if got := isQuota(tc.err); got != tc.quota {
				t.Errorf("isQuota = %v, want %v", got, tc.quota)
			}
			if got := errKind(tc.err); got != tc.wantErrKind {
				t.Errorf("errKind = %q, want %q", got, tc.wantErrKind)
			}
			if got := httpStatus(tc.err); got != tc.wantStatus {
				t.Errorf("httpStatus = %d, want %d", got, tc.wantStatus)
			}
		})
	}
}

// TestWrapCreateQuota verifies a quota error is wrapped so that errors.Is finds
// ErrQuota (for the executor's fail-fast) while the underlying response error
// stays in the chain (so classification still sees the 409).
func TestWrapCreateQuota(t *testing.T) {
	wrapped := wrapCreate(KindNetwork, "net-0001", respErr(409, `{"NeutronError":{"type":"OverQuota"}}`))
	if !errors.Is(wrapped, ErrQuota) {
		t.Errorf("wrapped quota error does not match ErrQuota: %v", wrapped)
	}
	if IsRetryable(wrapped) {
		t.Error("wrapped quota error must not be retryable")
	}

	plain := wrapCreate(KindNetwork, "net-0002", respErr(503, ""))
	if errors.Is(plain, ErrQuota) {
		t.Error("non-quota error must not match ErrQuota")
	}
	if !IsRetryable(plain) {
		t.Error("wrapped 503 error should remain retryable")
	}
}
