package devicestate

import (
	"errors"
	"os"
	"strings"
)

// ErrSnapshotsBlocked is returned instead of a snapshot when the node-agent's AppArmor profile blocks the snapshot mounts
// (Engine.SnapshotsBlocked). Nothing is diffed or frozen: every attempt would fail the same way until the node-agent restarts
// under another profile.
var ErrSnapshotsBlocked = errors.New("AppArmor blocks snapshots")

// AppArmorBlocksSnapshots reports whether label, the AppArmor label of the node-agent (the content of
// /proc/self/attr/current), is the container runtime's default profile in enforce mode. That profile has `deny mount`, and
// the snapshot filter mounts read-only overlay views of the device's layers, so every snapshot would fail with
// "permission denied".
func AppArmorBlocksSnapshots(label string) bool {
	name, mode, ok := strings.Cut(strings.TrimRight(label, "\x00\n "), " (")
	return ok && strings.HasPrefix(name, "cri-containerd") && mode == "enforce)"
}

// AppArmorLabel is the AppArmor label of this process; empty when AppArmor is off or the label cannot be read.
func AppArmorLabel() string {
	// The LSM-specific file first: with several LSMs stacked attr/current belongs to the first one only.
	for _, p := range []string{"/proc/self/attr/apparmor/current", "/proc/self/attr/current"} {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimRight(string(b), "\x00\n ")
		}
	}
	return ""
}
