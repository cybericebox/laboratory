package probe

import (
	"crypto/hmac"
	"crypto/sha256"
)

// The built-in observations of a lab check (docs/specs/2026-11-12-device-access-and-checks.md, section 4.1): directory, process, port,
// kernel_param, request and permission. The probe reads the state of the device ITSELF: it executes nothing of the device (no shell,
// no tool, no library of the image), it only reads /proc, /proc/sys, /etc and the file system, makes one connection (request), or
// asks the kernel whether a user may access a path (permission).
//
// The probe never gets an expected value. It is asked a question (a path, a port, a sysctl name, a user and a path, a destination)
// and answers with facts, bounded in size; the checker compares the facts with what the author expects. So a participant who reads
// the arguments of the probe learns what is looked at, never what is wanted.
//
// One subcommand, "check-observe <json>", answers one ObserveRequest with one ObserveResponse (the file and custom observations keep
// their own subcommands, check-file and check-exec).

const (
	// CommandCheckObserve is the subcommand of the built-in observations.
	CommandCheckObserve = "check-observe"
	// CommandCheckPerm is the child the probe starts for a permission test (see permission): never called by the checker.
	CommandCheckPerm = "check-perm"

	// The observation kinds.
	KindDirectory   = "directory"
	KindProcess     = "process"
	KindPort        = "port"
	KindKernelParam = "kernel_param"
	KindRequest     = "request"
	KindPermission  = "permission"

	// The bounds on what comes back.
	MaxDirHashes      = 32768    // entry names of a directory, as keyed hashes
	DirHashBytes      = 16       // length of one keyed hash
	MinDirKeyBytes    = 16       // the key the checker sends
	MaxProcessScan    = 1 << 20  // entries of the process table looked at
	MaxProcesses      = 2048     // processes returned
	MaxCmdlineBytes   = 512      // command line returned per process
	maxCmdlineRead    = 32 << 10 // command line read to find the first word
	MaxListeners      = 256      // sockets listed for one port
	MaxParamBytes     = 4096     // value of a sysctl
	MaxRequestBody    = 64 << 10 // body of an HTTP answer
	DefaultRequestMs  = 10000
	MaxRequestMs      = 600000
	requestPortMax    = 65535
	maxRequestPathLen = 2048
)

// ObserveRequest is one question. Only the fields of its Kind are used.
type ObserveRequest struct {
	Kind string `json:"kind"`
	// directory, permission
	Path string `json:"path,omitempty"`
	// directory: a random key for the keyed hashes of the entry names (see EntryHash). The names that the author expects are never sent.
	Key []byte `json:"key,omitempty"`
	// port, request
	Port     int    `json:"port,omitempty"`
	Protocol string `json:"protocol,omitempty"` // port: tcp | udp; request: tcp | http | https
	// kernel_param: the sysctl name with dots
	Name string `json:"name,omitempty"`
	// request
	Host      string `json:"host,omitempty"`
	Method    string `json:"method,omitempty"`
	URLPath   string `json:"urlPath,omitempty"`
	TimeoutMs int64  `json:"timeoutMs,omitempty"`
	// permission: a user name of the device, or a number
	User string `json:"user,omitempty"`
}

// ObserveResponse is the answer: Error is set when the question could not be answered, otherwise exactly the member of the kind.
type ObserveResponse struct {
	Error string `json:"error,omitempty"`

	Directory   *DirectoryFacts   `json:"directory,omitempty"`
	Processes   *ProcessFacts     `json:"processes,omitempty"`
	Port        *PortFacts        `json:"port,omitempty"`
	KernelParam *KernelParamFacts `json:"kernelParam,omitempty"`
	Request     *RequestFacts     `json:"request,omitempty"`
	Permission  *PermissionFacts  `json:"permission,omitempty"`
}

// DirectoryFacts: Exists is false when the path is missing; IsDir false when it is something else. Hashes are the keyed hashes
// (EntryHash, with the key of the question) of the names directly in the directory; Truncated says there were more than
// MaxDirHashes, so the set is not the whole directory. The checker hashes the names it expects with the same key and compares: the
// names the author expects never reach the device.
type DirectoryFacts struct {
	Exists    bool     `json:"exists"`
	IsDir     bool     `json:"isDir,omitempty"`
	Mode      uint32   `json:"mode,omitempty"`
	UID       uint32   `json:"uid,omitempty"`
	GID       uint32   `json:"gid,omitempty"`
	User      string   `json:"user,omitempty"`
	Group     string   `json:"group,omitempty"`
	Hashes    [][]byte `json:"hashes,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Process is one process of the device. Comm is the kernel's name (at most 15 characters); Arg0 the first word of the command line
// without its directory; Cmdline the command line, words joined by a space, cut at MaxCmdlineBytes (Cut says it was longer).
type Process struct {
	PID     int    `json:"pid"`
	Comm    string `json:"comm"`
	Arg0    string `json:"arg0,omitempty"`
	Cmdline string `json:"cmdline,omitempty"`
	Cut     bool   `json:"cut,omitempty"`
	UID     uint32 `json:"uid"`
	User    string `json:"user,omitempty"`
}

// ProcessFacts is the process table of the device (except the probe itself), at most MaxProcesses entries; Truncated says there were
// more. Incomplete counts the entries that could not be read (a denied read, a table over MaxProcessScan): they may be what is looked for.
type ProcessFacts struct {
	List       []Process `json:"list"`
	Truncated  bool      `json:"truncated,omitempty"`
	Incomplete int       `json:"incomplete,omitempty"`
}

// Listener is a socket that waits for connections (tcp: LISTEN; udp: bound and not connected) on the asked port. Addr is the local
// address ("0.0.0.0" and "::" are the wildcards). Procs are the names (Comm) of the processes that hold the socket.
type Listener struct {
	Protocol string   `json:"protocol"`
	Addr     string   `json:"addr"`
	Procs    []string `json:"procs,omitempty"`
	Arg0s    []string `json:"arg0s,omitempty"`
}

// PortFacts lists the listeners on the asked port and protocol.
type PortFacts struct {
	Listeners []Listener `json:"listeners"`
	Truncated bool       `json:"truncated,omitempty"`
	// OwnersIncomplete: some process could not be looked into, so a socket without an owner may have one.
	OwnersIncomplete bool `json:"ownersIncomplete,omitempty"`
}

// KernelParamFacts: Exists is false when there is no such sysctl.
type KernelParamFacts struct {
	Exists bool   `json:"exists"`
	Value  string `json:"value,omitempty"`
}

// RequestFacts: Connected is false when the connection did not open (Failure says how: refused, timeout, unreachable, resolve, tls or
// error). For http and https Status and Body are the answer (a redirect is an answer, not followed; the body is cut at
// MaxRequestBody).
type RequestFacts struct {
	Connected bool   `json:"connected"`
	Failure   string `json:"failure,omitempty"`
	Status    int    `json:"status,omitempty"`
	Body      []byte `json:"body,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// PermissionFacts: what the kernel says to the user, asked without opening anything and without changing anything. Exists is false
// when the path is missing (or cannot be reached by the user); UserFound false when the device has no such user.
type PermissionFacts struct {
	UserFound bool `json:"userFound"`
	Exists    bool `json:"exists"`
	Read      bool `json:"read"`
	Write     bool `json:"write"`
	Execute   bool `json:"execute"`
	// Error is set by the child of the test when the kernel gave an answer that is not a yes or a no.
	Error string `json:"error,omitempty"`
}

// EntryHash is the keyed hash of a directory entry name: HMAC-SHA256 truncated to DirHashBytes. The key is random per question, so the
// hash tells the device nothing about a name it did not already have, and the checker alone can say which hash is which name.
func EntryHash(key []byte, name string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(name))
	return m.Sum(nil)[:DirHashBytes]
}
