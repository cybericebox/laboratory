package devicestate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNoFreezer is returned when the container cgroup cannot be frozen (cgroup v1
// or an unknown cgroup path). The snapshot is then taken without freezing.
var ErrNoFreezer = errors.New("cgroup v2 freezer not available")

// CgroupDir turns the cgroup path from a container's OCI spec into its
// directory under the cgroup v2 mount at root. It understands both layouts the
// kubelet produces: the systemd driver ("kubepods-burstable-pod<uid>.slice:cri-containerd:<id>",
// whose slice name expands into nested slice directories) and cgroupfs
// ("/kubepods/burstable/pod<uid>/<id>"). It returns "" when the directory does not exist.
func CgroupDir(root, cgroupsPath string) string {
	if cgroupsPath == "" {
		return ""
	}
	var rel string
	if parts := strings.Split(cgroupsPath, ":"); len(parts) == 3 {
		slice, prefix, name := parts[0], parts[1], parts[2]
		unit := name
		if prefix != "" {
			unit = prefix + "-" + name
		}
		if !strings.HasSuffix(unit, ".scope") && !strings.HasSuffix(unit, ".slice") {
			unit += ".scope"
		}
		rel = filepath.Join(expandSlice(slice), unit)
	} else {
		rel = strings.TrimPrefix(cgroupsPath, "/")
	}
	dir := filepath.Join(root, rel)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// expandSlice expands a systemd slice name into its directory chain:
// "a-b-c.slice" is "a.slice/a-b.slice/a-b-c.slice"; "-.slice" and "" are the root.
func expandSlice(slice string) string {
	slice = strings.TrimSuffix(slice, ".slice")
	if slice == "" || slice == "-" {
		return ""
	}
	parts := strings.Split(slice, "-")
	var dirs []string
	for i := range parts {
		dirs = append(dirs, strings.Join(parts[:i+1], "-")+".slice")
	}
	return filepath.Join(dirs...)
}

// FindCgroup searches the cgroup tree for the directory of a container by id.
// The kubelet places it under a kubepods hierarchy; it is used when the path
// from the spec does not resolve.
func FindCgroup(root, containerID string) string {
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		if strings.Contains(d.Name(), containerID) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// Freeze freezes every process of the cgroup and returns the function that
// thaws it. The freeze is confirmed through cgroup.events before it returns.
func Freeze(ctx context.Context, dir string) (thaw func(), err error) {
	if dir == "" {
		return nil, ErrNoFreezer
	}
	freeze := filepath.Join(dir, "cgroup.freeze")
	if _, err := os.Stat(freeze); err != nil {
		return nil, ErrNoFreezer
	}
	if err := os.WriteFile(freeze, []byte("1"), 0o644); err != nil {
		return nil, fmt.Errorf("freeze %s: %w", dir, err)
	}
	thaw = func() { _ = os.WriteFile(freeze, []byte("0"), 0o644) }
	deadline := time.Now().Add(5 * time.Second)
	for {
		if frozen(dir) {
			return thaw, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			thaw()
			return nil, fmt.Errorf("freeze %s: not frozen after 5s", dir)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func frozen(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "frozen 1" {
			return true
		}
	}
	return false
}

// ThawOrphans thaws container cgroups left frozen, which happens when the
// node-agent dies in the middle of a snapshot. Nothing else freezes them.
// It returns the directories it thawed.
func ThawOrphans(root string) []string { return ThawOrphansExcept(root, nil) }

// ThawOrphansExcept preserves required captures that still own live tasks.
func ThawOrphansExcept(root string, protected map[string]bool) []string {
	var thawed []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), "cri-containerd-") || isContainerID(d.Name()) {
			if !protected[p] && frozen(p) {
				_ = os.WriteFile(filepath.Join(p, "cgroup.freeze"), []byte("0"), 0o644)
				thawed = append(thawed, p)
			}
			return filepath.SkipDir
		}
		return nil
	})
	return thawed
}

func isContainerID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
