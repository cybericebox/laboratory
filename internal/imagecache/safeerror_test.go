package imagecache

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/cybericebox/laboratory/internal/netguard"
)

const secretBody = "INTERNAL-SECRET token=abc123 10.0.0.7"

func TestSafeError(t *testing.T) {
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "reg.example.com", Path: "/v2/o/r/manifests/t"}}
	secretErr := &transport.Error{StatusCode: 404, Request: req, Errors: []transport.Diagnostic{{Code: "MANIFEST_UNKNOWN", Message: secretBody + "\nsecond line"}}}
	long := strings.Repeat("x", 5000)
	for name, c := range map[string]struct {
		err  error
		want string // the whole text; "" means only the properties below are checked
	}{
		"nil":                       {nil, ""},
		"a status with a body":      {secretErr, "registry reg.example.com answered 404 not found"},
		"the same, wrapped":         {fmt.Errorf("resolve nginx: %w", secretErr), "registry reg.example.com answered 404 not found"},
		"a status we have no words": {&transport.Error{StatusCode: 599, Request: req, Errors: []transport.Diagnostic{{Message: secretBody}}}, "registry reg.example.com answered 599 unexpected status"},
		"no request in the error":   {&transport.Error{StatusCode: 403}, "registry ? answered 403 forbidden"},
		"an internal redirect":      {fmt.Errorf("Get https://10.0.0.7/x: %w", netguard.ErrInternalAddress), ""},
		"a refused redirect":        {fmt.Errorf("%w: %s", netguard.ErrRedirectRefused, secretBody), ""},
		"a deadline":                {context.DeadlineExceeded, ""},
		"a dns failure":             {&net.DNSError{Err: secretBody, Name: "10.0.0.7"}, ""},
		"a certificate":             {x509.UnknownAuthorityError{}, ""},
		"a free text with controls": {errors.New("line one\nline two\r\n\x1b[31mred\x00\ttab " + secretBody), ""},
		"a very long text":          {errors.New(long), ""},
	} {
		got := SafeError(c.err)
		if c.err == nil {
			if got != "" {
				t.Errorf("%s: %q", name, got)
			}
			continue
		}
		if c.want != "" && got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
		if strings.ContainsAny(got, "\n\r\t\x00\x1b") {
			t.Errorf("%s: control characters in %q", name, got)
		}
		if len([]rune(got)) > 260 {
			t.Errorf("%s: %d characters", name, len([]rune(got)))
		}
	}
	for name, err := range map[string]error{
		"an internal redirect": fmt.Errorf("Get https://10.0.0.7/x: %w", netguard.ErrInternalAddress),
		"a refused redirect":   fmt.Errorf("%w: %s", netguard.ErrRedirectRefused, secretBody),
		"a dns failure":        &net.DNSError{Err: secretBody, Name: "10.0.0.7"},
		"a certificate":        x509.UnknownAuthorityError{},
		"a secret status":      &transport.Error{StatusCode: 500, Request: req, Errors: []transport.Diagnostic{{Message: secretBody}}},
	} {
		if got := SafeError(err); strings.Contains(got, "SECRET") || strings.Contains(got, "10.0.0.7") || strings.Contains(got, "abc123") {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := SafeError(fmt.Errorf("%w: x", netguard.ErrInternalAddress)); got != "a redirect to an address that is not allowed was refused" {
		t.Errorf("%q", got)
	}
	free := SafeError(errors.New("line one\nline two\r\n\x1b[31mred\x00\ttab"))
	if free != "line one line two [31mred tab" {
		t.Errorf("one line: %q", free)
	}
	if got := SafeError(errors.New(long)); len([]rune(got)) != maxSafeText+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("bounded: %d", len([]rune(got)))
	}
}
