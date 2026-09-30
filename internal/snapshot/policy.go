// Package snapshot holds the platform-independent logic of device state
// persistence: which paths are kept, the size quota, the layer chain rules and
// the tar plumbing that filters and squashes snapshot layers. It has no
// containerd or Kubernetes dependency in its pure parts; the registry side is
// in registry.go and image.go.
package snapshot

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// Defaults of the policy; the chart values and the operator configuration use
// the same numbers.
const (
	DefaultDebounce        = 5 * time.Second
	DefaultMaxSnapshotSize = int64(512 << 20)
	DefaultMaxLayers       = 10
)

// DefaultExcludePaths are never snapshotted unless the operator overrides the list.
var DefaultExcludePaths = []string{"/tmp", "/var/tmp", "/run"}

// SystemExcludePaths are always excluded, whatever the operator configures.
// They are the mount points and files the container runtime and kubelet create
// or bind over at every start (runtime filesystems, hostname and resolver
// files, the service account token directory), so a snapshot of them would only
// hold empty placeholders. Flag and secret values reach a device as environment
// variables from a Secret (envFrom) and never as files in the writable layer;
// a future file-based injection must add its path here.
var SystemExcludePaths = []string{
	"/dev", "/proc", "/sys",
	"/etc/hosts", "/etc/hostname", "/etc/resolv.conf", "/etc/mtab",
	"/var/run/secrets", "/run/secrets",
}

// Policy is the per-device snapshot policy.
type Policy struct {
	Debounce     time.Duration
	ExcludePaths []string
	MaxBytes     int64
	MaxLayers    int
}

// NewPolicy fills unset fields with the defaults and normalises the exclude
// list (absolute, cleaned, no duplicates), adding the system paths.
func NewPolicy(debounce time.Duration, exclude []string, maxBytes int64, maxLayers int) Policy {
	p := Policy{Debounce: debounce, MaxBytes: maxBytes, MaxLayers: maxLayers}
	if p.Debounce <= 0 {
		p.Debounce = DefaultDebounce
	}
	if p.MaxBytes <= 0 {
		p.MaxBytes = DefaultMaxSnapshotSize
	}
	if p.MaxLayers <= 0 {
		p.MaxLayers = DefaultMaxLayers
	}
	if exclude == nil {
		exclude = DefaultExcludePaths
	}
	seen := map[string]bool{}
	for _, e := range append(append([]string{}, exclude...), SystemExcludePaths...) {
		e = CleanPath(e)
		if e == "/" || seen[e] {
			continue
		}
		seen[e] = true
		p.ExcludePaths = append(p.ExcludePaths, e)
	}
	return p
}

// CleanPath returns the absolute, cleaned form of a path inside the container.
func CleanPath(p string) string {
	return path.Clean("/" + p)
}

// Excluded reports whether p, or one of its ancestors, is an excluded path.
func (p Policy) Excluded(name string) bool {
	name = CleanPath(name)
	for _, e := range p.ExcludePaths {
		if name == e || strings.HasPrefix(name, e+"/") {
			return true
		}
	}
	return false
}

// ErrQuota is returned when a snapshot would exceed the device's size quota.
var ErrQuota = errors.New("snapshot quota exceeded")

// CheckQuota reports ErrQuota (wrapped with the numbers) when the state already
// kept plus the new layer exceeds max. A zero max means no limit.
func CheckQuota(existing, added, max int64) error {
	if max > 0 && existing+added > max {
		return fmt.Errorf("%w: %d bytes kept + %d new > %d allowed", ErrQuota, existing, added, max)
	}
	return nil
}

// NeedSquash reports whether adding one more snapshot layer to a chain that has
// deltaLayers of them must squash the chain into one layer.
func NeedSquash(deltaLayers, maxLayers int) bool {
	return maxLayers > 0 && deltaLayers+1 > maxLayers
}

// BaseRepo is the registry repository that holds the base image layers shared
// by all devices, so a device's first snapshot mounts them instead of uploading.
const BaseRepo = "base"

// RepoPrefix is the top-level repository path of every lab's snapshots.
const RepoPrefix = "lab"

// Repo names the repository that holds one device's snapshots.
func Repo(namespace, lab, device string) string {
	return fmt.Sprintf("%s/%s/%s/%s", RepoPrefix, namespace, lab, device)
}

// LabRepoPrefix is the repository prefix shared by all devices of one lab.
func LabRepoPrefix(namespace, lab string) string {
	return fmt.Sprintf("%s/%s/%s/", RepoPrefix, namespace, lab)
}
