package grant

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DirKeys is a KeyLookup over a directory of PEM public keys, one file per key named <tenant>.<kid>: the ConfigMap the operator mirrors
// from the access key Secrets, mounted as files (pools-release.md, 2.3.4). The directory is read again at most every `every`: the kubelet
// updates a ConfigMap volume in place, so a key the platform removed stops working after the kubelet's delay plus at most that interval.
// A bad or half-written directory keeps the keys already held.
type DirKeys struct {
	dir   string
	every time.Duration
	now   func() time.Time

	mu      sync.Mutex
	keys    map[string]ed25519.PublicKey
	checked time.Time
	loaded  bool
}

// keyIDRE is what a key id may be inside a file name (the agent's accessKeyIDRE): no path separator. The name always starts with the
// tenant, which has no dot, so a dot-dot in a key id cannot name a parent directory.
var keyIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidKeyID says whether s can be the id of an access key in a file name of the directory.
func ValidKeyID(s string) bool { return keyIDRE.MatchString(s) && s != "." && s != ".." }

// NewDirKeys returns a lookup over dir. The directory is read at the first lookup.
func NewDirKeys(dir string, every time.Duration) *DirKeys {
	return &DirKeys{dir: dir, every: every, now: time.Now}
}

// Lookup is the KeyLookup of the directory.
func (d *DirKeys) Lookup(tenant, keyID string) (ed25519.PublicKey, bool) {
	if !ValidTenant(tenant) || !ValidKeyID(keyID) {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.loaded || d.now().Sub(d.checked) >= d.every {
		d.checked = d.now()
		if keys, err := readKeyDir(d.dir); err == nil {
			d.keys, d.loaded = keys, true
		}
	}
	k, ok := d.keys[tenant+"."+keyID]
	return k, ok
}

func readKeyDir(dir string) (map[string]ed25519.PublicKey, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]ed25519.PublicKey, len(entries))
	for _, e := range entries {
		name := e.Name()
		// the kubelet's own entries of an atomically updated volume (..data and the timestamped directory) are not keys
		if strings.HasPrefix(name, "..") || e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if k, ok := ParseAccessKey(raw); ok {
			keys[name] = k
		}
	}
	return keys, nil
}
