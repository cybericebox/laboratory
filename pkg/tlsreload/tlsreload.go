// Package tlsreload serves certificates that cert-manager renews in place: the
// key pair and the CA bundle are re-read from the mounted files when they change,
// so a renewal never needs a pod restart.
package tlsreload

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
)

// Files is a key pair and an optional CA bundle on disk. A change is detected by
// the modification time and size of the files (Secret volumes swap a symlink, and
// os.Stat follows it), checked on demand and at most once per MinInterval. A
// failed reload (for instance a half-written pair) keeps serving the last good
// material and is retried on the next check.
type Files struct {
	CertFile, KeyFile string
	// CAFile is the client CA (server side) or the server CA; empty if unused.
	CAFile string
	// MinInterval limits how often the files are stat'ed. Zero checks every time.
	MinInterval time.Duration

	mu        sync.Mutex
	cert      *tls.Certificate
	pool      *x509.CertPool
	sig       string
	lastCheck time.Time
	now       func() time.Time
}

// New loads the files once and fails if they are unusable.
func New(certFile, keyFile, caFile string) (*Files, error) {
	f := &Files{CertFile: certFile, KeyFile: keyFile, CAFile: caFile, MinInterval: 5 * time.Second, now: time.Now}
	if err := f.reload(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Files) signature() (string, error) {
	var sig string
	for _, p := range []string{f.CertFile, f.KeyFile, f.CAFile} {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		sig += fmt.Sprintf("%s|%d|%d;", p, st.ModTime().UnixNano(), st.Size())
	}
	return sig, nil
}

// reload must be called with f.mu held or before f is shared.
func (f *Files) reload() error {
	sig, err := f.signature()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
	if err != nil {
		return fmt.Errorf("load key pair: %w", err)
	}
	var pool *x509.CertPool
	if f.CAFile != "" {
		pem, err := os.ReadFile(f.CAFile)
		if err != nil {
			return fmt.Errorf("read CA: %w", err)
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("append CA %s", f.CAFile)
		}
	}
	f.cert, f.pool, f.sig = &cert, pool, sig
	return nil
}

// current returns the newest usable material.
func (f *Files) current() (*tls.Certificate, *x509.CertPool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if f.MinInterval > 0 && now.Sub(f.lastCheck) < f.MinInterval {
		return f.cert, f.pool
	}
	f.lastCheck = now
	if sig, err := f.signature(); err == nil && sig != f.sig {
		_ = f.reload() // keep the old material on failure; retried next check
	}
	return f.cert, f.pool
}

// GetCertificate serves the current server certificate.
func (f *Files) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, _ := f.current()
	return cert, nil
}

// GetClientCertificate presents the current client certificate.
func (f *Files) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	cert, _ := f.current()
	return cert, nil
}

// ServerConfig returns a tls.Config whose per-connection config always carries
// the current certificate and, when a CA file is set, requires a client
// certificate signed by the current CA. The per-connection config comes from
// GetConfigForClient, so NextProtos must be complete here (gRPC needs "h2").
func (f *Files) ServerConfig(nextProtos ...string) *tls.Config {
	build := func() *tls.Config {
		cert, pool := f.current()
		c := &tls.Config{
			Certificates: []tls.Certificate{*cert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   nextProtos,
		}
		if pool != nil {
			c.ClientAuth = tls.RequireAndVerifyClientCert
			c.ClientCAs = pool
		}
		return c
	}
	base := build()
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) { return build(), nil }
	return base
}
