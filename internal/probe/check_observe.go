package probe

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
	MaxDirEntries     = 4096     // names of a directory
	MaxDirNameBytes   = 255      // one name
	MaxProcesses      = 8192     // processes listed
	MaxCmdlineBytes   = 1024     // command line of one process
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

// DirectoryFacts: Exists is false when the path is missing; IsDir false when it is something else. Entries are the names directly in
// the directory (sorted, at most MaxDirEntries; Truncated says there were more).
type DirectoryFacts struct {
	Exists    bool     `json:"exists"`
	IsDir     bool     `json:"isDir,omitempty"`
	Mode      uint32   `json:"mode,omitempty"`
	UID       uint32   `json:"uid,omitempty"`
	GID       uint32   `json:"gid,omitempty"`
	User      string   `json:"user,omitempty"`
	Group     string   `json:"group,omitempty"`
	Entries   []string `json:"entries,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Process is one process of the device. Comm is the kernel's name (at most 15 characters); Arg0 the first word of the command line
// without its directory; Cmdline the whole command line, words joined by a space, cut at MaxCmdlineBytes.
type Process struct {
	PID     int    `json:"pid"`
	Comm    string `json:"comm"`
	Arg0    string `json:"arg0,omitempty"`
	Cmdline string `json:"cmdline,omitempty"`
	UID     uint32 `json:"uid"`
	User    string `json:"user,omitempty"`
}

// ProcessFacts lists the processes of the device except the probe itself (at most MaxProcesses).
type ProcessFacts struct {
	List      []Process `json:"list"`
	Truncated bool      `json:"truncated,omitempty"`
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
}
