package imagecache

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"unicode"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/cybericebox/laboratory/internal/netguard"
)

// maxSafeText bounds any text that reaches a status, an event or a log line from an error of unknown origin.
const maxSafeText = 200

// PinError is why a registry could not tell the digest of an image: the registry of the reference, the HTTP status it answered (0: none)
// and a reason in OUR words. It never carries text of the registry's answer (the body of a response, a message of a diagnostic, an address
// it redirected to): an upstream is not trusted with what an operator or a user reads in a Lab's status, in an event or in a log.
type PinError struct {
	Registry string
	Status   int
	Reason   string
}

func (e *PinError) Error() string {
	if e.Registry == "" {
		return e.Reason
	}
	if e.Status != 0 {
		return fmt.Sprintf("registry %s answered %d %s", e.Registry, e.Status, e.Reason)
	}
	return fmt.Sprintf("registry %s: %s", e.Registry, e.Reason)
}

// NewPinError describes err, an error of a request to registry, in words of our own.
func NewPinError(registry string, err error) *PinError {
	if pe, ok := classify(registry, err); ok {
		return pe
	}
	return &PinError{Registry: cleanText(registry, 100), Reason: "the request failed"}
}

// classify gives the words of our own for the errors a request to a registry can end in, and false for any other error.
func classify(registry string, err error) (*PinError, bool) {
	var pe *PinError
	if errors.As(err, &pe) {
		return pe, true
	}
	registry = cleanText(registry, 100)
	var te *transport.Error
	switch {
	case errors.As(err, &te):
		reason := strings.ToLower(http.StatusText(te.StatusCode))
		if reason == "" {
			reason = "unexpected status"
		}
		return &PinError{Registry: registry, Status: te.StatusCode, Reason: reason}, true
	case errors.Is(err, netguard.ErrInternalAddress), errors.Is(err, netguard.ErrRedirectRefused):
		return &PinError{Registry: registry, Reason: "a redirect to an address that is not allowed was refused"}, true
	case errors.Is(err, context.Canceled):
		return &PinError{Registry: registry, Reason: "the request was canceled"}, true
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return &PinError{Registry: registry, Reason: "the request timed out"}, true
	}
	var dns *net.DNSError
	var unknownCA x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.As(err, &dns):
		return &PinError{Registry: registry, Reason: "the name did not resolve"}, true
	case errors.As(err, &unknownCA), errors.As(err, &host), errors.As(err, &invalid), errors.As(err, &verify):
		return &PinError{Registry: registry, Reason: "the certificate was not accepted"}, true
	}
	return nil, false
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// SafeError is the text of err that may be written to the status of a Lab or a group, to an event or to a log: a PinError as it is (or one
// made from a transport error), and for any other error its text on one line, without control characters, at most 200 characters. Use
// it wherever an error that may come from an upstream registry is shown.
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	registry := ""
	var te *transport.Error
	if errors.As(err, &te) {
		registry = "?"
		if te.Request != nil && te.Request.URL != nil {
			registry = te.Request.URL.Host
		}
	}
	if pe, ok := classify(registry, err); ok {
		return pe.Error()
	}
	return cleanText(err.Error(), maxSafeText)
}

// cleanText is s on one line: control characters and line breaks become one space each (runs of white space collapse), and it is cut to
// max runes with an ellipsis.
func cleanText(s string, max int) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		if n >= max {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
