package registrygate

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"testing"
)

// redirectRegistry makes the rig's upstream answer every manifest with a 307 to https://<host>/..., and makes the
// names resolve to the test CDN (the dial is redirected: no name leaves the loopback).
func (r *rig) redirectRegistry(host string) {
	r.t.Helper()
	r.up.mu.Lock()
	r.up.redirectManifests, r.up.redirectHost = true, host
	r.up.mu.Unlock()
	tr := r.cache.Up.HTTP.Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		if _, port, _ := net.SplitHostPort(addr); port == "443" {
			addr = r.up.cdn.Listener.Addr().String()
		}
		return d.DialContext(ctx, network, addr)
	}
	r.cache.Up.HTTP.Transport = tr
}

func (r *rig) noAuthReachedTheCDN() {
	r.t.Helper()
	r.up.mu.Lock()
	defer r.up.mu.Unlock()
	if len(r.up.authSeen) == 0 {
		r.t.Fatal("the redirect target was never called")
	}
	for _, a := range r.up.authSeen {
		if a != "" {
			r.t.Fatalf("an Authorization header reached the redirect target: %q", a)
		}
	}
}

// registry.k8s.io: every manifest is a 307 to a regional host. The redirect proves the manifest, and the fill that follows reaches the target without the credentials.
func TestRedirectedManifestIsProvedAndServed(t *testing.T) {
	r := newRig(t)
	layer := bytes.Repeat([]byte("L"), 1024)
	ld := r.up.put("library/nginx", "blobs", layer)
	cfg := r.up.put("library/nginx", "blobs", []byte("{}"))
	md := r.up.put("library/nginx", "manifests", manifestBody(cfg, ld))
	r.redirectRegistry("example.com") // the test CDN's certificate names example.com

	code, body, hdr := r.pull("GET", mpath("library/nginx", md), "", "", nil)
	if code != 200 || hdr.Get("Docker-Content-Digest") != md || !bytes.Contains(body, []byte(ld)) {
		t.Fatalf("manifest: %d %v", code, hdr)
	}
	r.noAuthReachedTheCDN()
	for _, d := range []string{ld, cfg} {
		if code, _, _ := r.pull("GET", bpath("library/nginx", d), "", "", nil); code != 200 {
			t.Fatalf("blob %s: %d", d, code)
		}
	}
	if !r.inZot("c/up/library/nginx", "manifests", md) {
		t.Fatal("the image must be stored in zot")
	}
	// the same with credentials of a tenant: the registry (here the redirecting one) is asked, the target gets no Authorization
	if code, _, _ := r.pull("GET", mpath("library/nginx", md), alice, "pw", nil); code != 200 {
		t.Fatalf("with credentials: %d", code)
	}
	r.noAuthReachedTheCDN()
}
