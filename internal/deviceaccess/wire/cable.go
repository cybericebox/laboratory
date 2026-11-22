package wire

import (
	"encoding/json"
)

// Console lines (docs/internal-docs/laboratory/specs/console-ports-design.md, 6.5). Two contracts live here:
//
//   - cable.sock, the unix socket between the node-agent of a node and the device access service of the same node. The node-agent puts the
//     whole set of lines of its node (LinesRequest) and reads their state back (LinesReply). It names cables and the ports of their ends;
//     there is no field for a program, a container or a user: the service runs only "probe port-attach <port>" in a lab device it found
//     itself.
//   - the cable stream between the device access services of two nodes (CablePath, a WebSocket on the pod network): the owner of a line
//     dials the service of the node of the serial end. The first message is CableOpen, authenticated with an HMAC of a key only the device
//     access pods hold, with a time and a nonce; the reply is authenticated the same way. Then binary messages both ways: Data frames with
//     the raw bytes of the line and one Close frame with a reason.

// LinesPath is the endpoint of cable.sock: PUT sets the lines of the node (the body is a LinesRequest), GET reads their state; both
// answer a LinesReply.
const LinesPath = "/v1/lines"

// CablePath is where the device access service accepts the cable stream of another node.
const CablePath = "/v1/cable"

// RelayVersion is the version of the line relay a service speaks. The node-agent publishes the feature only when the service of its node
// answers with this version or a later one.
const RelayVersion = 1

// Roles of a line on a node.
const (
	// RoleOwner: the console end runs on this node; the service attaches it, attaches the serial end too when it runs here (Peer empty),
	// else dials Peer, and moves the bytes.
	RoleOwner = "owner"
	// RoleSerial: only the serial end runs on this node; the service accepts the owner's stream for this line and nothing else.
	RoleSerial = "serial"
)

// Reasons of a line that is not up (the reasons of the ConsoleCable's Ready condition).
const (
	LineWaitingForDevice = "WaitingForDevice"
	LinePortDown         = "PortDown"
	LinePeerUnreachable  = "PeerUnreachable"
)

// LineEnd is one end of a line: a device of the lab, the port, and the device's whole port list (the JSON of the keeper's list) whose
// digest the operator stamped on the pod (names.LabelPorts). The service recomputes the digest and refuses a list that differs, so the
// node-agent cannot name a port the device does not have.
type LineEnd struct {
	Device string          `json:"device"`
	Port   string          `json:"port"`
	Ports  json.RawMessage `json:"ports"`
}

// LineSpec is one line as the node-agent sees it.
type LineSpec struct {
	// Namespace, Name and UID are those of the ConsoleCable.
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	// Lab and LabUID are the Lab of both ends: a device is attached only when its pod carries both.
	Lab     string  `json:"lab"`
	LabUID  string  `json:"labUID"`
	Serial  LineEnd `json:"serial"`
	Console LineEnd `json:"console"`
	Role    string  `json:"role"`
	// Peer (owner only) is host:port of the device access pod on the node of the serial end; empty when the serial end runs on this node.
	Peer string `json:"peer,omitempty"`
}

// LinesRequest is the whole set of lines of the node. A line that is not in it is stopped.
type LinesRequest struct {
	Lines []LineSpec `json:"lines"`
}

// LineState is what the service knows of one line.
type LineState struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Role      string `json:"role"`
	// ConsoleAttached and SerialAttached: an attach to the keeper of that end runs (for a serial-role line only SerialAttached is known).
	ConsoleAttached bool `json:"consoleAttached,omitempty"`
	SerialAttached  bool `json:"serialAttached,omitempty"`
	// Up: both ends are attached and the bytes move.
	Up bool `json:"up,omitempty"`
	// Reason is one of the Line reasons while the line is not up.
	Reason string `json:"reason,omitempty"`
}

// LinesReply answers both methods of LinesPath.
type LinesReply struct {
	Relay int         `json:"relay"`
	Lines []LineState `json:"lines"`
	Error string      `json:"error,omitempty"`
}

// PortRef names a port of a device in a CableOpen.
type PortRef struct {
	Device string `json:"device"`
	Port   string `json:"port"`
}

// CableOpen is the first message of a cable stream. Every field but MAC is covered by MAC (see the deviceaccess package, cable_auth.go).
type CableOpen struct {
	V         int     `json:"v"`
	Namespace string  `json:"namespace"`
	Name      string  `json:"name"`
	UID       string  `json:"uid"`
	Lab       string  `json:"lab"`
	LabUID    string  `json:"labUID"`
	Serial    PortRef `json:"serial"`
	Console   PortRef `json:"console"`
	// Time is the dialer's clock in Unix milliseconds; Nonce is 16 random bytes in hex, never accepted twice.
	Time  int64  `json:"time"`
	Nonce string `json:"nonce"`
	MAC   string `json:"mac"`
}

// CableReply answers a CableOpen. Nonce repeats the nonce of the request and MAC covers it with OK and Error, so the dialer knows the
// answer comes from a holder of the key and belongs to its request.
type CableReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Nonce string `json:"nonce"`
	MAC   string `json:"mac"`
}

// CodePortDown is the refusal of a cable stream whose serial port cannot be attached now (busy, no keeper, the port failed).
const CodePortDown = "port-down"
