package devicestate

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Forward listens on listen (a loopback address) and relays every connection
// to target. The node-agent uses it to make the cluster-internal snapshot
// registry reachable as localhost:<port> from the node's container runtime,
// which treats localhost registries as plain HTTP, so no host configuration
// (registries.yaml, hosts.toml, certificates, DNS) is needed. It returns when
// ctx ends.
//
// With a reader (WithReader) it is an HTTP-aware relay instead: the registry lets only the platform read the snapshots of
// the labs (`lab/**`) and the shared `base` repository (everything else is anonymous, the public image cache), so
// the node's runtime, which pulls a snapshot as localhost:<port>/lab/..., has the reader account added for it here, on
// GET and HEAD requests of those repositories that carry no credentials of their own. Writes are never given the reader.
func Forward(ctx context.Context, listen, target string, log logr.Logger, opts ...ForwardOption) error {
	var o forwardOptions
	for _, f := range opts {
		f(&o)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if o.readerUser != "" {
		return serveReader(ctx, ln, target, o, log)
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			relay(ctx, c, target, log)
		}()
	}
}

func relay(ctx context.Context, client net.Conn, target string, log logr.Logger) {
	defer client.Close()
	d := net.Dialer{Timeout: 5 * time.Second}
	upstream, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		log.V(1).Info("registry forwarder: dial upstream", "target", target, "err", err.Error())
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(upstream, client)
	go cp(client, upstream)
	select {
	case <-done:
		<-done
	case <-ctx.Done():
	}
}

// ForwardOption configures Forward.
type ForwardOption func(*forwardOptions)

type forwardOptions struct{ readerUser, readerPassword, host string }

// WithReader makes the forwarder add the reader account to the reads of the snapshot repositories.
func WithReader(user, password string) ForwardOption {
	return func(o *forwardOptions) { o.readerUser, o.readerPassword = user, password }
}

// WithHost sets the exact Host header a request must carry to be given the reader account: the address the node's
// runtime pulls the snapshots from (localhost:<port>). A request that arrives under any other Host is relayed without it.
func WithHost(host string) ForwardOption {
	return func(o *forwardOptions) { o.host = host }
}

// needsReader says whether a registry request reads a repository only the platform may read.
func needsReader(method, path string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	if strings.Contains(path, "..") || strings.Contains(path, "//") {
		return false
	}
	return strings.HasPrefix(path, "/v2/lab/") || strings.HasPrefix(path, "/v2/base/")
}

// readerHandler relays registry requests to target, adding the reader account where needsReader says so.
func readerHandler(target string, o forwardOptions) http.Handler {
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			hostOK := o.host == "" || strings.EqualFold(r.Host, o.host)
			r.URL.Scheme, r.URL.Host, r.Host = "http", target, target
			if hostOK && r.Header.Get("Authorization") == "" && needsReader(r.Method, r.URL.Path) {
				r.SetBasicAuth(o.readerUser, o.readerPassword)
			}
		},
		FlushInterval: -1, // blobs stream
		ErrorHandler:  func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) },
	}
	return proxy
}

func serveReader(ctx context.Context, ln net.Listener, target string, o forwardOptions, log logr.Logger) error {
	srv := &http.Server{Handler: readerHandler(target, o), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		log.Error(err, "registry forwarder")
		return err
	}
	return nil
}
