package glance

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
		{"quota 413", respErr(413, "Image quota exceeded"), "quota"},
		{"quota 403 body", respErr(403, "Denied: over quota"), "quota"},
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
		// Glance signals an over-the-limit request with 413.
		{"413 over limit", respErr(413, "anything"), true},
		{"403 with quota body", respErr(403, "Image count quota exceeded"), true},
		{"403 with limit body", respErr(403, "Attribute limit reached"), true},
		{"403 without quota body", respErr(403, "policy forbids this"), false},
		{"400 with quota body", respErr(400, "over quota"), true},
		// A 409 conflict is a lifecycle conflict, never a quota, so it stays
		// retryable.
		{"409 with quota-like body", respErr(409, "quota exceeded"), false},
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
		{"409 conflict", respErr(409, "conflict"), true},
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

func TestResourceNameAndIdentity(t *testing.T) {
	if got, want := resourceName("abc123", "img-0001"), "dizzy-abc123-img-0001"; got != want {
		t.Errorf("resourceName = %q, want %q", got, want)
	}
	tags := runTags("abc123")
	if len(tags) != 2 || tags[0] != "dizzy:run=abc123" || tags[1] != "dizzy:type=image" {
		t.Errorf("runTags = %v, want [dizzy:run=abc123 dizzy:type=image]", tags)
	}
}
