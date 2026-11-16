package imagecache

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PlatformTenant is the tenant the operator writes into the username of the platform's own pull credentials (the VPN and gateway
// images of a private registry): they belong to no tenant, and the gate needs a name there.
const PlatformTenant = "platform"

// RekeyForCache turns dockerconfigjson documents (the raw pull Secrets: keys are upstream registry hosts) into the one document a pod
// of a lab that uses the image cache carries. The kubelet asks the cache, not the registry, so a credential is keyed by the cache path of
// its registry, "<prefix>/c/<registry>" (the kubelet matches a key by registry host and the path prefix of the image), and the username
// becomes "<tenant>~<upstream username>": the kubelet sends nothing but the credentials, so the tenant travels in the name. Only the
// operator makes this document, from the tenant label of the lab and never from anything the caller sends; the gate splits the name
// before any upstream call.
//
// A credential for a registry the cache does not serve is dropped (such an image is refused, see RewriteRequired), and so is one without
// a username (the gate needs both parts of the name). Of two documents the later one wins for the same registry: pass the tenant's
// credentials first and the lab's own last. It returns the document and how many registries it holds; none gives a nil document.
func RekeyForCache(prefix, tenant string, served []string, docs ...[]byte) ([]byte, int, error) {
	if prefix == "" || tenant == "" || strings.ContainsAny(tenant, "~:/ ") {
		return nil, 0, fmt.Errorf("re-key for the cache: need a cache prefix and a tenant name, got %q and %q", prefix, tenant)
	}
	allowed := map[string]bool{}
	for _, s := range served {
		allowed[normalizeHost(s)] = true
	}
	type auth struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Auth     string `json:"auth"`
	}
	out := map[string]auth{}
	for _, doc := range docs {
		if len(doc) == 0 {
			continue
		}
		var cfg struct {
			Auths map[string]struct {
				Username string `json:"username"`
				Password string `json:"password"`
				Auth     string `json:"auth"`
			} `json:"auths"`
		}
		if err := json.Unmarshal(doc, &cfg); err != nil {
			return nil, 0, fmt.Errorf("dockerconfigjson: %w", err)
		}
		hosts := make([]string, 0, len(cfg.Auths))
		for h := range cfg.Auths {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts) // the same input always gives the same document
		for _, host := range hosts {
			a := cfg.Auths[host]
			reg := normalizeHost(host)
			if !allowed[reg] {
				continue
			}
			user, pass := a.Username, a.Password
			if user == "" && a.Auth != "" {
				raw, err := base64.StdEncoding.DecodeString(a.Auth)
				if err != nil {
					return nil, 0, fmt.Errorf("dockerconfigjson auth of %s: %w", host, err)
				}
				user, pass, _ = strings.Cut(string(raw), ":")
			}
			if user == "" || pass == "" {
				continue
			}
			name := tenant + "~" + user
			out[prefix+"/"+CacheSegment+"/"+reg] = auth{Username: name, Password: pass, Auth: base64.StdEncoding.EncodeToString([]byte(name + ":" + pass))}
		}
	}
	if len(out) == 0 {
		return nil, 0, nil
	}
	raw, err := json.Marshal(struct {
		Auths map[string]auth `json:"auths"`
	}{out})
	return raw, len(out), err
}

// WithoutUsername lists the registries (as the cache names them, sorted) of a dockerconfigjson document whose entry carries no username,
// served by the cache or not: the cache cannot carry such a credential, because the tenant is written into the username.
func WithoutUsername(doc []byte) ([]string, error) {
	if len(doc) == 0 {
		return nil, nil
	}
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(doc, &cfg); err != nil {
		return nil, fmt.Errorf("dockerconfigjson: %w", err)
	}
	var out []string
	for host, a := range cfg.Auths {
		user := a.Username
		if user == "" && a.Auth != "" {
			if raw, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
				user, _, _ = strings.Cut(string(raw), ":")
			}
		}
		if user == "" {
			out = append(out, normalizeHost(host))
		}
	}
	sort.Strings(out)
	return out, nil
}
