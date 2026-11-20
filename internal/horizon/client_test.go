package horizon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"syscall"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

func TestErrKind(t *testing.T) {
	keystone := func(code int) error { return gophercloud.ErrUnexpectedResponseCode{Actual: code} }
	horizon := func(code int) error { return &StatusError{Step: "page x", Status: code} }
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("x: %w", ErrRejected), "rejected"},
		{fmt.Errorf("x: %w", ErrMismatch), "mismatch"},
		{fmt.Errorf("x: %w: %w", ErrForbidden, keystone(403)), "http_403"},
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "canceled"},
		{horizon(302), "http_302"},
		{horizon(405), "http_405"},
		{horizon(500), "http_500"},
		{horizon(418), "http_other"},
		{keystone(404), "http_404"},
		{keystone(409), "http_409"},
		{keystone(507), "http_other"},
		{&url.Error{Op: "Get", URL: "http://x", Err: syscall.ECONNREFUSED}, "other"},
		{errors.New("boom"), "other"},
	} {
		if got := errKind(tc.err); got != tc.want {
			t.Errorf("errKind(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&StatusError{Status: 429}, true},
		{&StatusError{Status: 503}, true},
		{gophercloud.ErrUnexpectedResponseCode{Actual: 500}, true},
		{&StatusError{Status: 404}, false},
		{&StatusError{Status: 302}, false},
		{&url.Error{Op: "Get", URL: "http://x", Err: syscall.ECONNREFUSED}, true},
		{io.ErrUnexpectedEOF, true},
		{&url.Error{Op: "Get", URL: "http://x", Err: context.DeadlineExceeded}, false},
		{context.Canceled, false},
		{fmt.Errorf("%w: %w", ErrMismatch, &StatusError{Status: 503}), false},
		{fmt.Errorf("%w", ErrRejected), false},
		{fmt.Errorf("%w: %w", ErrForbidden, gophercloud.ErrUnexpectedResponseCode{Actual: 403}), false},
		{errors.New("boom"), false},
	} {
		if got := IsRetryable(tc.err); got != tc.want {
			t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestPageKind(t *testing.T) {
	if got := PageKind("api_access"); got != "page_api_access" {
		t.Errorf("PageKind = %q, want page_api_access", got)
	}
}
