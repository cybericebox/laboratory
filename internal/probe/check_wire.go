package probe

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

// The in-device side of the lab checks. The checker on the node asks the device, through the container runtime's exec, to run
// "probe check-exec <json>" (a custom observation) or "probe check-file <json>" (a file observation of a micro-VM device, whose
// root file system is not visible from the node). The request is the one JSON argument and the answer is one JSON document on
// the standard output, so an exit code of the tool never gets confused with the exit code of the checked command.

const (
	// CommandCheckExec and CommandCheckFile are the two subcommands of the tool.
	CommandCheckExec = "check-exec"
	CommandCheckFile = "check-file"

	// MaxCheckOutput bounds the output of a checked command that comes back (stdout and stderr together).
	MaxCheckOutput = 64 << 10
	// MaxCheckFile bounds the content of a file that comes back.
	MaxCheckFile = 1 << 20
	// MaxIDFile bounds /etc/passwd and /etc/group when names are looked up.
	MaxIDFile = 256 << 10
)

// ExecRequest is a custom observation as the tool receives it.
type ExecRequest struct {
	// Argv is the command (the first item is looked up in PATH); empty when Script is set.
	Argv []string `json:"argv,omitempty"`
	// Script is run by Interpreter; it is handed over as a file, not as an argument.
	Script      string `json:"script,omitempty"`
	Interpreter string `json:"interpreter,omitempty"`
	// User is a name from /etc/passwd of the device, or a number (uid or uid:gid).
	User string `json:"user,omitempty"`
	// TimeoutMs kills the command (its process group) when it runs longer.
	TimeoutMs int64 `json:"timeoutMs"`
}

// ExecResponse is the outcome of a custom observation.
type ExecResponse struct {
	// Started is false when the command could not be started (no such file, no permission, no such user).
	Started  bool   `json:"started"`
	Exit     int    `json:"exit"`
	Signal   string `json:"signal,omitempty"`
	TimedOut bool   `json:"timedOut,omitempty"`
	// Output is stdout and stderr in the order they were written, cut at MaxCheckOutput.
	Output    []byte `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Error says why the command did not start; it never repeats output.
	Error string `json:"error,omitempty"`
}

// FileRequest is a file observation of a device whose file system the node cannot see.
type FileRequest struct {
	Path        string `json:"path"`
	WantContent bool   `json:"wantContent,omitempty"`
}

// FileResponse is what the tool saw. Symbolic links are followed (inside the device, which the participant controls anyway).
type FileResponse struct {
	Exists  bool   `json:"exists"`
	Regular bool   `json:"regular,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	UID     uint32 `json:"uid,omitempty"`
	GID     uint32 `json:"gid,omitempty"`
	User    string `json:"user,omitempty"`
	Group   string `json:"group,omitempty"`
	// Content is the start of the file (at most MaxCheckFile bytes) when it was asked for and the file is regular.
	Content   []byte `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}

// IDNames are the user and group names of a device by number, read from its /etc/passwd and /etc/group.
type IDNames struct {
	Users  map[uint32]string
	Groups map[uint32]string
}

// ParseIDNames reads the first field and the third field of the lines of passwd and group (the first line for a number wins).
func ParseIDNames(passwd, group []byte) IDNames {
	return IDNames{Users: parseIDFile(passwd), Groups: parseIDFile(group)}
}

func parseIDFile(raw []byte) map[uint32]string {
	out := map[uint32]string{}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		f := strings.Split(string(line), ":")
		if len(f) < 3 || f[0] == "" || strings.HasPrefix(f[0], "#") {
			continue
		}
		id, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		if _, dup := out[uint32(id)]; !dup {
			out[uint32(id)] = f[0]
		}
	}
	return out
}

// readBounded reads at most limit bytes of a plain file of the tool's own root (the files of /etc), nil when it cannot.
func readBounded(path string, limit int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, limit)
	n, _ := f.Read(buf)
	return buf[:n]
}

func writeJSON(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 1
	}
	if _, err := os.Stdout.Write(raw); err != nil {
		return 1
	}
	return 0
}

// RunCheckCommand runs check-exec or check-file with its one JSON argument and returns the process exit code: 0 whenever an
// answer was written (the answer carries the outcome), 2 for a usage error.
func RunCheckCommand(cmd string, args []string) int {
	if len(args) != 1 {
		_, _ = os.Stderr.WriteString("usage: probe " + cmd + " <json>\n")
		return 2
	}
	switch cmd {
	case CommandCheckExec:
		var req ExecRequest
		if err := json.Unmarshal([]byte(args[0]), &req); err != nil {
			_, _ = os.Stderr.WriteString("bad request\n")
			return 2
		}
		return writeJSON(runCheckExec(req))
	case CommandCheckObserve:
		var req ObserveRequest
		if err := json.Unmarshal([]byte(args[0]), &req); err != nil {
			_, _ = os.Stderr.WriteString("bad request\n")
			return 2
		}
		return writeJSON(Observe(req))
	case CommandCheckPerm:
		return RunPermChild(args[0])
	case CommandCheckFile:
		var req FileRequest
		if err := json.Unmarshal([]byte(args[0]), &req); err != nil {
			_, _ = os.Stderr.WriteString("bad request\n")
			return 2
		}
		return writeJSON(runCheckFile(req))
	}
	return 2
}
