package devicestate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

// Forward listens on listen (a loopback address) and relays the registry requests of the node's container runtime and the kubelet to
// target, the registry gate. The node-agent uses it to make the cluster-internal registry reachable as localhost:<port>, which the
// runtime treats as plain HTTP, so no host configuration (registries.yaml, hosts.toml, certificates, DNS) is needed.
//
// The relay is HTTP-aware and has the identity of the node and nothing else to give: every request it forwards carries the projected
// ServiceAccount token of the node-agent (WithNodeToken) in X-Cib-Node-Token, and no header the client sent is believed: every X-Cib-*
// header and the client's Authorization header are removed. There is no credential on the node and none is needed: the gate decides by
// the node and the reference (the tenant is in the image path).
//
// This listener serves pulls only: GET and HEAD on /v2/, /v2/c/** (the image cache) and /v2/lab/** (the snapshots); anything else is
// answered with 405 before it reaches the gate. Any process on the node can connect to a loopback port, so what arrives here must be no
// more than what the container runtime gets. Pushes of snapshots go through the unix socket of WithPushSocket (ServePush).
// It returns when ctx ends.
func Forward(ctx context.Context, listen, target string, log logr.Logger, opts ...ForwardOption) error {
	var o forwardOptions
	for _, f := range opts {
		f(&o)
	}
	if o.token == nil {
		return errors.New("registry relay: no node token configured (WithNodeToken)")
	}
	if o.tlsCAFile != "" {
		if err := o.loadTLS(target); err != nil {
			return err
		}
	}
	tr := o.upstreamTransport()
	scheme := "http"
	if o.tlsConf != nil {
		scheme = "https"
	}
	pull := &http.Server{Handler: relayHandler(scheme, target, tr, o.token, pullAllowed), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	serve := func(srv *http.Server, ln net.Listener, what string) {
		defer wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(err, "registry relay", "listener", what)
			errs <- err
		}
	}
	servers := []*http.Server{pull}
	wg.Add(1)
	go serve(pull, ln, "tcp")
	if o.pushSocket != "" {
		pln, err := listenPushSocket(o.pushSocket, o.pushSocketUID)
		if err != nil {
			_ = pull.Close()
			wg.Wait()
			return err
		}
		push := &http.Server{Handler: relayHandler(scheme, target, tr, o.token, pushAllowed), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, push)
		wg.Add(1)
		go serve(push, pln, "push socket")
	}
	var result error
	select {
	case <-ctx.Done():
	case result = <-errs:
	}
	for _, s := range servers {
		_ = s.Close()
	}
	wg.Wait()
	if o.pushSocket != "" {
		_ = os.Remove(o.pushSocket)
	}
	return result
}

// ForwardOption configures Forward.
type ForwardOption func(*forwardOptions)

type forwardOptions struct {
	// tlsCAFile makes the relay speak TLS to the registry, trusting only that certificate authority (PEM).
	tlsCAFile string
	tlsConf   *tls.Config
	token     *imagecache.TokenFile
	// pushSocket is the path of the unix socket of the snapshot push, pushSocketUID the owner given to it (-1: leave the owner).
	pushSocket    string
	pushSocketUID int
	// transport is the transport towards the registry (tests); nil builds one from the TLS configuration.
	transport http.RoundTripper
}

// WithTLS makes the forwarder dial the registry over TLS, trusting only the certificate authority in caFile; the name the server's
// certificate must carry is the host of the target.
func WithTLS(caFile string) ForwardOption {
	return func(o *forwardOptions) { o.tlsCAFile = caFile }
}

// WithNodeToken gives every request the projected ServiceAccount token in tokenFile (audience laboratory-registry-gate), read again every
// imagecache.TokenReread, in the header X-Cib-Node-Token. A relay without it does not start: it would have no identity.
func WithNodeToken(tokenFile string) ForwardOption {
	return func(o *forwardOptions) { o.token = imagecache.NewTokenFile(tokenFile) }
}

// WithPushSocket makes the relay serve the snapshot push of node-state on a unix socket at path (mode 0600, owned by uid when uid is not
// negative: the user of node-state), apart from the TCP listener that cannot be told from any other process of the node.
func WithPushSocket(path string, uid int) ForwardOption {
	return func(o *forwardOptions) { o.pushSocket, o.pushSocketUID = path, uid }
}

func (o *forwardOptions) loadTLS(target string) error {
	pem, err := os.ReadFile(o.tlsCAFile)
	if err != nil {
		return fmt.Errorf("registry CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("registry CA %s holds no certificate", o.tlsCAFile)
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	o.tlsConf = &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}
	return nil
}

func (o *forwardOptions) upstreamTransport() http.RoundTripper {
	if o.transport != nil {
		return o.transport
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if o.tlsConf != nil {
		tr.TLSClientConfig = o.tlsConf
	}
	return tr
}

// A rule says whether the relay forwards a request; the answer is the status the relay itself gives when it does not.
type rule func(r *http.Request) (refusal int)

// pullAllowed is the rule of the TCP listener: GET and HEAD of /v2/, /v2/c/** and /v2/lab/**.
func pullAllowed(r *http.Request) int {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return http.StatusMethodNotAllowed
	}
	p, ok := cleanRequestPath(r)
	if !ok {
		return http.StatusMethodNotAllowed
	}
	if p == "/v2/" || strings.HasPrefix(p, "/v2/c/") || strings.HasPrefix(p, "/v2/lab/") {
		return 0
	}
	return http.StatusMethodNotAllowed
}

// pushAllowed is the rule of the push socket: POST, PATCH, PUT, GET and HEAD under /v2/lab/<namespace>/<lab>/<device>/, nothing else. No
// DELETE: the nodes have none, the gate deletes what a new snapshot replaced.
func pushAllowed(r *http.Request) int {
	switch r.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodGet, http.MethodHead:
	default:
		return http.StatusMethodNotAllowed
	}
	p, ok := cleanRequestPath(r)
	if !ok {
		return http.StatusMethodNotAllowed
	}
	rest, ok := strings.CutPrefix(p, "/v2/lab/")
	if !ok {
		return http.StatusMethodNotAllowed
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return http.StatusMethodNotAllowed
	}
	return 0
}

// cleanRequestPath is the path of a request when it is in its plain form: no empty or dot element, no encoded slash, dot or backslash,
// no control character. What the relay checks must be what the gate reads.
func cleanRequestPath(r *http.Request) (string, bool) {
	p := r.URL.Path
	raw := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(raw, "%2f") || strings.Contains(raw, "%2e") || strings.Contains(raw, "%5c") || strings.ContainsAny(p, "\\\x00\r\n") {
		return "", false
	}
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "//") {
		return "", false
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == "." || elem == ".." {
			return "", false
		}
	}
	return p, true
}

// relayHandler forwards what the rule lets through to target, with the identity of the node: the client's Authorization and every X-Cib-*
// header are removed and the token is set. A token that cannot be read is a 503, never a request without identity.
func relayHandler(scheme, target string, transport http.RoundTripper, token *imagecache.TokenFile, allow rule) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = scheme, target, target
			pr.Out.Header.Del("Authorization")
			imagecache.StripPlatformHeaders(pr.Out.Header)
			tok, _ := token.Token() // checked before; a token read again in between that fails leaves the request without it, and the gate refuses it
			pr.Out.Header.Set(imagecache.HeaderNodeToken, tok)
		},
		FlushInterval: -1, // blobs stream
		ErrorHandler:  func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) },
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := allow(r); status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		if _, err := token.Token(); err != nil {
			http.Error(w, "the node has no token for the registry", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// listenPushSocket creates the unix socket of the snapshot push: mode 0600, the owner given. A socket left by an earlier run is removed;
// anything else at the path is an error.
func listenPushSocket(sock string, uid int) (net.Listener, error) {
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("push socket %s: the path exists and is not a socket", sock)
		}
		if err := os.Remove(sock); err != nil {
			return nil, fmt.Errorf("push socket %s: %w", sock, err)
		}
	}
	if err := os.MkdirAll(path.Dir(sock), 0o755); err != nil {
		return nil, fmt.Errorf("push socket %s: %w", sock, err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("push socket %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("push socket %s: %w", sock, err)
	}
	if uid >= 0 {
		if err := os.Chown(sock, uid, -1); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("push socket %s: %w", sock, err)
		}
	}
	return ln, nil
}

// UnixTransport is a transport that dials the unix socket at sock whatever the URL says: the client keeps the logical host of the registry
// (localhost:<port>) in every name and reference, and the socket is how it reaches the relay.
func UnixTransport(sock string) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}
	return tr
}
