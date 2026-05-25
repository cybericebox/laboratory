package l7

import (
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// BackendResolver maps (task, groupID) to a backend URL string.
type BackendResolver func(task, groupID string) (string, error)

var validTaskRE = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)

type Handler struct {
	pubKey     ed25519.PublicKey
	baseDomain string
	cookieName string
	resolver   BackendResolver
	transport  http.RoundTripper
}

// upstreamTransport accepts self-signed certificates on lab instance Services.
// Spec §5: "https (re-encrypt) — прокси заново шифрует к инстансу (свой серт
// инстанса, можно self-signed)". The hop is in-cluster (ClusterIP Service),
// trust boundary is the NetworkPolicy that admits only proxy-system/app=proxy.
var upstreamTransport = &http.Transport{
	TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, // #nosec G402 — see comment above
	ForceAttemptHTTP2: true,
	MaxIdleConns:      100,
	IdleConnTimeout:   90 * time.Second,
}

func NewHandler(pubKey ed25519.PublicKey, baseDomain, cookieName string, resolver BackendResolver) *Handler {
	return &Handler{
		pubKey:     pubKey,
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

	groupID, err := validateCookie(r, h.pubKey, h.cookieName)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	backendURL, err := h.resolver(task, groupID)
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
		ns := fmt.Sprintf("labgroup-%s", groupID)
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
