// Package probe is the readiness probe of a device pod. It is a tiny static program, copied by an init container into
// an emptyDir that the device container mounts read-only, because a device image may carry no shell and no curl. The
// kubelet runs it as an exec readiness probe inside the container, so it sees the container's own network namespace
// and file system.
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	SchemeTCP   = "tcp"
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"

	// InstalledName is the file name of the copy in the shared directory.
	InstalledName = "probe"

	// dialTimeout bounds one connection attempt; the kubelet's own timeout bounds the whole probe.
	dialTimeout = 800 * time.Millisecond
	// requestTimeout bounds the HTTP exchange on one address.
	requestTimeout = 1500 * time.Millisecond
)

// Check is what makes a device ready. Every set part must pass; an empty Check is an error, not success.
type Check struct {
	// File is a marker the application creates; the check passes once it exists.
	File string
	// Port is a port the application must answer on, with Scheme tcp (a connection is accepted), http or https (an HTTP
	// answer with a status below 500: any answer proves the application serves; 4xx is a page behind a login, not a
	// failure).
	Port   int
	Scheme string
}

// Run checks the marker and the port. addrs lists the addresses of the container to try for the port (see LocalAddrs).
func (c Check) Run(ctx context.Context, addrs func() []string) error {
	if c.File == "" && c.Port == 0 {
		return errors.New("nothing to check")
	}
	if c.File != "" {
		if _, err := os.Stat(c.File); err != nil {
			return fmt.Errorf("marker file: %w", err)
		}
	}
	if c.Port == 0 {
		return nil
	}
	var last error
	for _, host := range addrs() {
		err := c.checkPort(ctx, host)
		if err == nil {
			return nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no address to try")
	}
	return last
}

func (c Check) checkPort(ctx context.Context, host string) error {
	addr := net.JoinHostPort(host, strconv.Itoa(c.Port))
	switch c.Scheme {
	case SchemeHTTP, SchemeHTTPS:
		return httpAnswers(ctx, c.Scheme, addr)
	default:
		d := net.Dialer{Timeout: dialTimeout}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}
}

func httpAnswers(ctx context.Context, scheme, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: dialTimeout}
			return d.DialContext(ctx, network, addr)
		},
		// The certificate of a lab application is self-signed and the probe talks to the container itself.
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // a readiness probe of the local container
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		// A redirect is an answer; where it leads (an external login page) is not the probe's business.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://localhost/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "cybericebox-readiness-probe")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// LocalAddrs are the addresses the probe tries: the loopback first, then every global unicast address of the container
// (an application may listen only on the pod's own address).
func LocalAddrs() []string {
	out := []string{"127.0.0.1"}
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range ifaces {
		ipn, ok := a.(*net.IPNet)
		if !ok || !ipn.IP.IsGlobalUnicast() {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	return out
}

// Install copies the program src to dir/InstalledName, read-only and executable by everybody, so the device container
// can run it as any user and cannot change it. The copy is atomic: a rerun of the init container never leaves half a file.
func Install(src, dir string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o555); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, InstalledName))
}
