// Command gate is the stand-in of the registry gate for the P0 spike (see
// docs/research/2026-11-16-zot-tenant-cache.md, section 11): a plain-HTTP registry on the loopback of a kind node
// that answers 401 with a Basic challenge to a request without credentials, checks the Basic credentials of the
// others against a list (one registry each) and serves one image built at start: a manifest, a config and one layer
// that holds the sleeper binary. It logs every request as a JSON line, which is what the test reads: it proves what
// containerd and the kubelet really send. It stores and forwards nothing.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

type credential struct{ registry, user, pass string }

type credFlags []credential

func (c *credFlags) String() string { return fmt.Sprint(*c) }

// Set takes registry=user:password.
func (c *credFlags) Set(v string) error {
	registry, rest, ok := strings.Cut(v, "=")
	user, pass, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || registry == "" || user == "" {
		return fmt.Errorf("want registry=user:password, got %q", v)
	}
	*c = append(*c, credential{registry, user, pass})
	return nil
}

type blob struct {
	data []byte
	mime string
}

type server struct {
	creds  credFlags
	blobs  map[string]blob
	digest string // of the manifest
	mu     sync.Mutex
	out    *os.File
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func buildImage(sleeper []byte) (map[string]blob, string, error) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	hdr := &tar.Header{Name: "sleeper", Mode: 0o755, Size: int64(len(sleeper)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, "", err
	}
	if _, err := tw.Write(sleeper); err != nil {
		return nil, "", err
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(tarBuf.Bytes()); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	config, _ := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH, "os": "linux",
		"config": map[string]any{"Entrypoint": []string{"/sleeper"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{sum(tarBuf.Bytes())}},
	})
	const (
		cfgMime   = "application/vnd.oci.image.config.v1+json"
		layerMime = "application/vnd.oci.image.layer.v1.tar+gzip"
		manMime   = "application/vnd.oci.image.manifest.v1+json"
	)
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": manMime,
		"config": map[string]any{"mediaType": cfgMime, "digest": sum(config), "size": len(config)},
		"layers": []any{map[string]any{"mediaType": layerMime, "digest": sum(gz.Bytes()), "size": gz.Len()}},
	})
	blobs := map[string]blob{
		sum(config): {config, cfgMime}, sum(gz.Bytes()): {gz.Bytes(), layerMime}, sum(manifest): {manifest, manMime},
	}
	return blobs, sum(manifest), nil
}

type entry struct {
	Time   string `json:"time"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth"` // none, basic, other
	User   string `json:"user,omitempty"`
	Status int    `json:"status"`
	Kind   string `json:"kind"` // ping, manifest, blob, other
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }

func kindOf(path string) string {
	switch {
	case path == "/v2/" || path == "/v2":
		return "ping"
	case strings.Contains(path, "/manifests/"):
		return "manifest"
	case strings.Contains(path, "/blobs/"):
		return "blob"
	}
	return "other"
}

func (s *server) log(r *http.Request, user, auth string, status int) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	line, _ := json.Marshal(entry{now, r.Method, r.URL.Path, auth, user, status, kindOf(r.URL.Path)})
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintf(s.out, "%s\n", line)
}

// registryOf is <registry> of /v2/c/<registry>/<repo>/(manifests|blobs)/<ref>.
func registryOf(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 5 || parts[0] != "v2" || parts[1] != "c" {
		return ""
	}
	return parts[2]
}

func (s *server) allowed(registry, user, pass string) bool {
	for _, c := range s.creds {
		if c.registry == registry && c.user == user && c.pass == pass {
			return true
		}
	}
	return false
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	user, pass, hasBasic := r.BasicAuth()
	auth := "none"
	switch {
	case hasBasic:
		auth = "basic"
	case r.Header.Get("Authorization") != "":
		auth = "other"
	}
	defer func() { s.log(r, user, auth, rec.status) }()

	if kindOf(r.URL.Path) == "ping" {
		rec.WriteHeader(http.StatusOK)
		return
	}
	registry := registryOf(r.URL.Path)
	if registry == "" {
		http.NotFound(rec, r)
		return
	}
	if !hasBasic || !s.allowed(registry, user, pass) {
		rec.Header().Set("WWW-Authenticate", `Basic realm="p0-gate"`)
		rec.WriteHeader(http.StatusUnauthorized)
		return
	}
	ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if strings.Contains(r.URL.Path, "/manifests/") && !strings.HasPrefix(ref, "sha256:") {
		ref = s.digest // a tag resolves to the one manifest
	}
	b, ok := s.blobs[ref]
	if !ok {
		http.NotFound(rec, r)
		return
	}
	rec.Header().Set("Content-Type", b.mime)
	rec.Header().Set("Content-Length", fmt.Sprint(len(b.data)))
	rec.Header().Set("Docker-Content-Digest", ref)
	rec.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = rec.Write(b.data)
	}
}

func main() {
	var creds credFlags
	listen := flag.String("listen", "127.0.0.1:5035", "address")
	sleeperPath := flag.String("sleeper", "", "the binary of the image (linux, static)")
	logPath := flag.String("log", "/var/log/p0-gate.log", "request log (JSON lines)")
	digestPath := flag.String("digest-file", "/var/log/p0-gate.digest", "where the manifest digest is written")
	flag.Var(&creds, "cred", "registry=user:password, repeatable")
	flag.Parse()

	sleeper, err := os.ReadFile(*sleeperPath)
	if err != nil {
		log.Fatal(err)
	}
	blobs, digest, err := buildImage(sleeper)
	if err != nil {
		log.Fatal(err)
	}
	out, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*digestPath, []byte(digest), 0o600); err != nil {
		log.Fatal(err)
	}
	s := &server{creds: creds, blobs: blobs, digest: digest, out: out}
	log.Printf("p0 gate on %s, image %s", *listen, digest)
	srv := &http.Server{Addr: *listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
