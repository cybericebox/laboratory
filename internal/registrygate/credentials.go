package registrygate

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"

	"github.com/cybericebox/laboratory/internal/names"
)

// LabRef names a lab whose own credentials may be needed.
type LabRef struct{ Namespace, Lab string }

// CredentialSource gives the upstream credentials of a tenant segment, read by the gate and nobody else (2.4.5). The tenant is taken from
// the image path, never from anything a caller sends.
type CredentialSource interface {
	// Candidates returns the credentials to try, in order, for registry reg: a lab's own credentials first (for each lab that names the
	// reference on the calling node), then the tenant's (or the platform's). One empty Creds (anonymous) when none of them has an entry
	// for the registry: the image is proved public.
	Candidates(tenant string, reg Registry, labs []LabRef) []Creds
}

// SecretView is the CredentialSource over the Secrets of the namespace laboratory-upstream, which a watch keeps in memory (kubesource.go
// feeds it; tests set it directly): tenant-<name>, platform, lab-<hash>. The value of a Secret is its .dockerconfigjson document.
type SecretView struct {
	mu      sync.RWMutex
	secrets map[string][]byte
}

// NewSecretView is an empty view.
func NewSecretView() *SecretView { return &SecretView{secrets: map[string][]byte{}} }

// Set stores (or replaces) the dockerconfigjson of a Secret.
func (v *SecretView) Set(name string, dockerConfigJSON []byte) {
	v.mu.Lock()
	v.secrets[name] = append([]byte(nil), dockerConfigJSON...)
	v.mu.Unlock()
}

// Delete forgets a Secret.
func (v *SecretView) Delete(name string) {
	v.mu.Lock()
	delete(v.secrets, name)
	v.mu.Unlock()
}

func (v *SecretView) get(name string) ([]byte, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	b, ok := v.secrets[name]
	return b, ok
}

// Candidates implements CredentialSource.
func (v *SecretView) Candidates(tenant string, reg Registry, labs []LabRef) []Creds {
	var out []Creds
	seen := map[Creds]bool{}
	add := func(secret string) {
		data, ok := v.get(secret)
		if !ok {
			return
		}
		if c, ok := dockerConfigCreds(data, reg); ok && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	if tenant != names.SegmentPlatform {
		for _, l := range labs {
			add(names.UpstreamLabSecret(l.Namespace, l.Lab))
		}
		add(names.UpstreamTenantSecret(tenant))
	} else {
		add(names.UpstreamPlatformSecret)
	}
	if len(out) == 0 {
		return []Creds{{}} // no entry for this registry: proved anonymously
	}
	return out
}

// dockerConfigCreds finds the entry of registry reg in a dockerconfigjson document (by name or by the host of its URL; Docker Hub's
// aliases are docker.io).
func dockerConfigCreds(data []byte, reg Registry) (Creds, bool) {
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return Creds{}, false
	}
	for host, a := range cfg.Auths {
		h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
		if h == "index.docker.io" || h == "registry-1.docker.io" {
			h = "docker.io"
		}
		if h != reg.Name && (reg.URL == nil || h != reg.URL.Host) {
			continue
		}
		user, pass := a.Username, a.Password
		if user == "" && a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				continue
			}
			user, pass, _ = strings.Cut(string(raw), ":")
		}
		if user != "" {
			return Creds{User: user, Pass: pass}, true
		}
	}
	return Creds{}, false
}
