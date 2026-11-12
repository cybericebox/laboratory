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
	MaxDirNames       = 256      // names asked of one directory
	MaxDirNameBytes   = 255      // one name
	MaxProcessScan    = 1 << 20  // entries of the process table looked at
	MaxCmdlineBytes   = 32 << 10 // command line looked at (a longer one makes the process incomplete)
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
	// directory: the entry names to look for directly in the directory (each is answered by lstat, so the answer never depends on
	// the size of the directory)
	Names []string `json:"names,omitempty"`
	// port, request
	Port     int    `json:"port,omitempty"`
	Protocol string `json:"protocol,omitempty"` // port: tcp | udp; request: tcp | http | https
	// kernel_param: the sysctl name with dots. process: the command name to look for.
	Name string `json:"name,omitempty"`
	// process: a regular expression on the command line; the user (name or number) the process must belong to. A process is a match
	// when every given key holds; the whole process table is scanned and only the result comes back.
	Matches string `json:"matches,omitempty"`
	// request
	Host      string `json:"host,omitempty"`
	Method    string `json:"method,omitempty"`
	URLPath   string `json:"urlPath,omitempty"`
	TimeoutMs int64  `json:"timeoutMs,omitempty"`
	// permission, process: a user name of the device, or a number
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

// DirectoryFacts: Exists is false when the path is missing; IsDir false when it is something else. Present are the asked names that
// exist directly in the directory (by lstat).
type DirectoryFacts struct {
	Exists  bool     `json:"exists"`
	IsDir   bool     `json:"isDir,omitempty"`
	Mode    uint32   `json:"mode,omitempty"`
	UID     uint32   `json:"uid,omitempty"`
	GID     uint32   `json:"gid,omitempty"`
	User    string   `json:"user,omitempty"`
	Group   string   `json:"group,omitempty"`
	Present []string `json:"present,omitempty"`
}

// Process is one process of the device as the matcher sees it. Comm is the kernel's name (at most 15 characters); Arg0 the first word of
// the command line without its directory; Cmdline the command line, words joined by a space.
type Process struct {
	PID     int    `json:"pid"`
	Comm    string `json:"comm"`
	Arg0    string `json:"arg0,omitempty"`
	Cmdline string `json:"cmdline,omitempty"`
	UID     uint32 `json:"uid"`
	User    string `json:"user,omitempty"`
}

// ProcessFacts: the program scanned the process table (except itself) and Found says that a process matched. Incomplete counts what it
// could not see completely (an entry it was not allowed to read, a command line longer than MaxCmdlineBytes, a table over
// MaxProcessScan): when nothing matched and Incomplete is not zero, the absence is not known.
type ProcessFacts struct {
	Found      bool `json:"found"`
	Scanned    int  `json:"scanned"`
	Incomplete int  `json:"incomplete,omitempty"`
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
