package registrygate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"time"
)

// RegistrySet is the list of upstream registries the gate may call, replaced as a whole when the file it comes from changes (the
// ConfigMap of the chart): no restart of the gate and none of zot, which carries no registry list.
type RegistrySet struct {
	v atomic.Pointer[map[string]Registry]
}

// Get returns the registry by the name an image carries.
func (s *RegistrySet) Get(name string) (Registry, bool) {
	m := s.v.Load()
	if m == nil {
		return Registry{}, false
	}
	r, ok := (*m)[name]
	return r, ok
}

// Set replaces the list.
func (s *RegistrySet) Set(list []Registry) {
	m := make(map[string]Registry, len(list))
	for _, r := range list {
		m[r.Name] = r
	}
	s.v.Store(&m)
}

// Names are the registries in the list.
func (s *RegistrySet) Names() []string {
	m := s.v.Load()
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(*m))
	for n := range *m {
		out = append(out, n)
	}
	return out
}

// ParseRegistries reads [{"name": "ghcr.io", "url": "https://ghcr.io", "authHosts": ["auth.example"], "redirects": ["*.pkg.dev"]}, ...] (authHosts and redirects optional; redirects are the hosts a manifest request may be redirected to, see ParseRedirectPatterns; a bad pattern refuses the whole list). A URL is https, or http for a registry inside the cluster.
func ParseRegistries(data []byte) ([]Registry, error) {
	var raw []struct {
		Name, URL string
		AuthHosts []string
		Redirects []string
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("registries: %w", err)
	}
	out := make([]Registry, 0, len(raw))
	for _, e := range raw {
		u, err := url.Parse(e.URL)
		if err != nil || e.Name == "" || u.Host == "" || u.Scheme != "https" && u.Scheme != schemeHTTP {
			return nil, fmt.Errorf("registries: bad entry %q %q", e.Name, e.URL)
		}
		hosts := e.AuthHosts
		if e.Name == "docker.io" && len(hosts) == 0 {
			hosts = []string{"auth.docker.io"} // Hub's challenge names its token service
		}
		redirects, err := ParseRedirectPatterns(e.Redirects)
		if err != nil {
			return nil, fmt.Errorf("registries: %q: %w", e.Name, err)
		}
		out = append(out, Registry{Name: e.Name, URL: u, AuthHosts: hosts, Redirects: redirects})
	}
	return out, nil
}

// Watch loads the file now and again whenever its content changes (checked every interval: a ConfigMap volume is updated by swapping a
// symlink, which a poll sees and an inotify watch on the file does not). A file that cannot be read or parsed keeps the list in force.
func (s *RegistrySet) Watch(ctx context.Context, path string, interval time.Duration, onError func(error)) error {
	var last [32]byte
	load := func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if bytes.Equal(sum[:], last[:]) {
			return nil
		}
		list, err := ParseRegistries(data)
		if err != nil {
			return err
		}
		s.Set(list)
		last = sum
		return nil
	}
	if err := load(); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := load(); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
	return nil
}
