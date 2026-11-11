// Package launcher is the first process of a device container that has a bootstrap list or a service command of its own.
// It is a mode of the static in-device program (cmd/probe), so the image needs neither a shell nor a libc.
//
// With a list it places the files, runs the script under its time limit, erases the list and only then replaces itself
// (exec) with the service, under the service user, so that process 1 of the container is the service. The list reaches the
// container through a memory-backed volume (names.BootstrapDir) and is deleted before the service starts; the Secret that
// holds it is never mounted into the device container. Without a list (a pod that starts from a saved state, or a device
// with only a service command) it just starts the service.
package launcher

import (
	"encoding/json"
	"os"
)

// Exit codes of the launcher. The service has its own codes once it runs; these are only seen when it never started.
const (
	// ExitFailed: a bootstrap attempt failed. The termination message says which attempt and whether it was the last.
	ExitFailed = 78
	// ExitRecycle: the container was restarted in place after the service ended. The bootstrap list is gone, the filesystem
	// is not clean any more, so the pod has to be recreated (a new pod runs the init containers again).
	ExitRecycle = 79
	// ExitStart: the service command could not be started.
	ExitStart = 127
	// ExitUsage: the launcher was started wrongly.
	ExitUsage = 2
)

// Termination message kinds.
const (
	KindFailed  = "failed"
	KindRecycle = "recycle"
	KindStart   = "start"
)

// MaxReasonBytes bounds the reason (the Device status holds 256 bytes).
const MaxReasonBytes = 200

// Message is what the launcher leaves in the container termination message when it ends without starting the service.
// The reason is a fixed phrase with numbers: it never contains file paths, contents, script text, script output or values.
type Message struct {
	Kind    string `json:"kind"`
	Attempt int    `json:"attempt,omitempty"`
	Final   bool   `json:"final,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ParseMessage reads a termination message; ok is false for anything that is not the launcher's.
func ParseMessage(s string) (Message, bool) {
	var m Message
	if json.Unmarshal([]byte(s), &m) != nil {
		return Message{}, false
	}
	switch m.Kind {
	case KindFailed, KindRecycle, KindStart:
		return m, true
	}
	return Message{}, false
}

// DefaultTerminationLog is where the kubelet reads the termination message.
const DefaultTerminationLog = "/dev/termination-log"

func (m Message) write(path string) {
	if path == "" {
		return
	}
	if len(m.Reason) > MaxReasonBytes {
		m.Reason = m.Reason[:MaxReasonBytes]
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644) //nolint:gosec // the kubelet reads it, it holds no secret
}
