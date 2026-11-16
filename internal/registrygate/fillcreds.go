package registrygate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// AnonymousFill holds the optional credential per upstream registry that the gate uses to fill an image it proved readable WITHOUT
// credentials (rate-limit relief, for example a Docker Hub login). It is used for nothing else: never for a tenant's private image,
// never to prove anything. It is lab-wide, not a tenant credential.
type AnonymousFill map[string]Creds

// ParseAnonymousFill reads a dockerconfigjson document and keeps the entries of the registries in the list (by name or by URL host;
// Docker Hub's aliases index.docker.io and registry-1.docker.io are docker.io).
func ParseAnonymousFill(data []byte, regs []Registry) (AnonymousFill, error) {
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("anonymous fill credentials: %w", err)
	}
	out := AnonymousFill{}
	for host, a := range cfg.Auths {
		user, pass := a.Username, a.Password
		if user == "" && a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return nil, fmt.Errorf("anonymous fill credentials for %s: %w", host, err)
			}
			user, pass, _ = strings.Cut(string(raw), ":")
		}
		if user == "" {
			continue
		}
		h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
		if u, err := url.Parse("//" + h); err == nil && u.Host != "" {
			h = u.Host
		}
		if h == "index.docker.io" || h == "registry-1.docker.io" {
			h = "docker.io"
		}
		for _, r := range regs {
			if r.Name == h || r.URL.Host == h {
				out[r.Name] = Creds{User: user, Pass: pass}
			}
		}
	}
	return out, nil
}

// fillCreds are the credentials a fill uses: the pull's own, or, for an image proved public, the registry's anonymous-fill login.
func (c *Cache) fillCreds(who caller, registry string) (Creds, string) {
	if who.hash != "" {
		return who.creds, who.hash
	}
	if cr, ok := c.Fill[registry]; ok {
		return cr, "anonymous-fill|" + registry
	}
	return Creds{}, ""
}
