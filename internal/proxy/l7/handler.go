package l7

import (
	"crypto/rsa"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// BackendResolver maps (task, groupID) to a backend URL string.
type BackendResolver func(task, groupID string) (string, error)

var validTaskRE = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)

type Handler struct {
	key        func() *rsa.PublicKey
	baseDomain string
	cookieName string
	resolver   BackendResolver
	transport  http.RoundTripper
}

// upstreamTransport skips certificate verification for in-cluster backends
// that may use self-signed certificates. The trust boundary is the
// NetworkPolicy that admits only laboratory-proxy/app=proxy.
var upstreamTransport = &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, // #nosec G402 — see comment above
	MaxIdleConns:    100,
	IdleConnTimeout: 90 * time.Second,
}

func NewHandler(key func() *rsa.PublicKey, baseDomain, cookieName string, resolver BackendResolver) *Handler {
	return &Handler{
		key:        key,
		baseDomain: baseDomain,
		cookieName: cookieName,
		resolver:   resolver,
		transport:  upstreamTransport,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if colonIdx := strings.IndexByte(host, ':'); colonIdx >= 0 {
		host = host[:colonIdx]
	}
	suffix := "." + h.baseDomain
	if !strings.HasSuffix(host, suffix) {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	task := strings.TrimSuffix(host, suffix)
	if !validTaskRE.MatchString(task) {
		http.Error(w, "invalid task", http.StatusBadRequest)
		return
	}

	claims, err := validateCookie(r, h.key, h.cookieName)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	backendURL, err := h.resolver(task, claims.GroupID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	target, err := url.Parse(backendURL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Strip challenge cookie before forwarding.
	r = r.Clone(r.Context())
	var kept []string
	for _, c := range r.Cookies() {
		if c.Name != h.cookieName {
			kept = append(kept, c.Name+"="+c.Value)
		}
	}
	r.Header.Set("Cookie", strings.Join(kept, "; "))
	r.Header.Set("X-Forwarded-Proto", "https")

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = h.transport
	proxy.ServeHTTP(w, r)
}

// ServiceResolver builds a BackendResolver that determines backend URL from Service named port.
// getServiceProtocol(task, namespace) returns the Service's first port name ("http" or "https").
func ServiceResolver(getServiceProtocol func(task, namespace string) (string, error)) BackendResolver {
	return func(task, groupID string) (string, error) {
		ns := laboratoryv1alpha1.LabGroupNamespace(groupID)
		proto, err := getServiceProtocol(task, ns)
		if err != nil {
			return "", err
		}
		port := 80
		if proto == "https" {
			port = 443
		}
		return fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", proto, task, ns, port), nil
	}
}
