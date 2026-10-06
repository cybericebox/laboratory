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
	DefaultDebounce   = 5 * time.Second
	DefaultWriteQuota = int64(512 << 20)
	DefaultMaxLayers  = 10
	// DefaultMaxFileSize: a regular file larger than this is left out of a snapshot.
	DefaultMaxFileSize = int64(256 << 20)
	// DefaultMaxEntries is the most entries (files, directories, links) one snapshot layer may hold. The byte quota counts
	// only the bytes of regular files, so a million empty files cost nothing against it but a lot of memory and time to
	// merge and unpack; this is the cap on them.
	DefaultMaxEntries = 100000
	// MaxEntryHeaderBytes is the most the names and extended attributes of one entry may take; an entry over it is left out.
	MaxEntryHeaderBytes = 8192
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
	WriteQuota   int64
	// MaxFileSize: a regular file over it is skipped (like an excluded path, for that file only).
	MaxFileSize int64
	MaxLayers   int
	// MaxEntries caps the entries of one snapshot layer (and of a squashed chain); over it the snapshot is refused (the last good
	// one is kept and the status carries a warning), like the byte quota.
	MaxEntries int
}

// NewPolicy fills unset fields with the defaults and normalises the exclude
// list (absolute, cleaned, no duplicates), adding the system paths.
func NewPolicy(debounce time.Duration, exclude []string, maxBytes int64, maxLayers int) Policy {
	p := Policy{Debounce: debounce, WriteQuota: maxBytes, MaxLayers: maxLayers}
	if p.Debounce <= 0 {
		p.Debounce = DefaultDebounce
	}
	if p.WriteQuota <= 0 {
		p.WriteQuota = DefaultWriteQuota
	}
	p.MaxFileSize = DefaultMaxFileSize
	p.MaxEntries = DefaultMaxEntries
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

// WithMaxFileSize sets the per-file size limit (n <= 0 keeps the default).
func (p Policy) WithMaxFileSize(n int64) Policy {
	if n > 0 {
		p.MaxFileSize = n
	}
	return p
}

// WithMaxEntries sets the entry cap (n <= 0 keeps the default).
func (p Policy) WithMaxEntries(n int) Policy {
	if n > 0 {
		p.MaxEntries = n
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
var ErrQuota = errors.New("write quota exceeded")

// ErrEntries is returned when a snapshot layer (or the squashed chain) holds more entries than the policy allows.
var ErrEntries = errors.New("too many files in the snapshot")

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
