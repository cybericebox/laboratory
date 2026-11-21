package wire

// The checker socket. The lab checker of a node asks the device access service, over a local unix socket, to run the in-device probe with
// one of the check subcommands in a lab device. The request names where to look and one subcommand with its one JSON argument; there is no
// field for a container, a program, a user or an environment, so nothing but the probe can be run, and always as root in the device (the
// probe itself drops to the user of a custom check).

// CheckerPath is the path of the one endpoint of the checker socket.
const CheckerPath = "/v1/probe"

// ProbeRequest is one run of the probe in a device.
type ProbeRequest struct {
	Namespace string `json:"namespace"`
	Lab       string `json:"lab"`
	Device    string `json:"device"`
	// Command is one of the check subcommands of the probe (check-exec, check-file, check-observe); Arg is its one JSON argument.
	Command string `json:"command"`
	Arg     string `json:"arg"`
	// TimeoutMs is how long the probe may run before it is killed; the service lowers it to its own ceiling.
	TimeoutMs int64 `json:"timeoutMs"`
}

// ProbeResponse is what the probe printed. Error is a short code (the Code constants, or "timeout"/"busy"); Stdout is the probe's one JSON
// document, which the checker judges and never publishes.
type ProbeResponse struct {
	Error  string `json:"error,omitempty"`
	Exit   int    `json:"exit"`
	Stdout []byte `json:"stdout,omitempty"`
}

// CodeTimeout and CodeForbidden are the codes only the checker socket uses.
const (
	CodeTimeout   = "timeout"
	CodeForbidden = "forbidden"
)
