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
//     dials the service of the node of the serial end. Each device access process has its own Ed25519 identity, made at start and kept in
//     memory; its node-agent publishes the public key on the Node, and gives each service the keys of the peers of its lines. The first
//     message is CableOpen, signed by the dialer with an ephemeral X25519 key, a time and a nonce; the reply is signed by the acceptor
//     with its own ephemeral key. The session keys come from the X25519 exchange of the two ephemeral keys, bound to both signed messages.
//     Then every binary message is sealed (AES-256-GCM, a key and a sequence number per direction): Data frames with the raw bytes of the
//     line and one Close frame.

// LinesPath is the endpoint of cable.sock: PUT sets the lines of the node (the body is a LinesRequest), GET reads their state; both
// answer a LinesReply.
const LinesPath = "/v1/lines"

// CablePath is where the device access service accepts the cable stream of another node.
const CablePath = "/v1/cable"

// RelayVersion is the version of the line relay a service speaks. The node-agent publishes the feature only when the service of its node
// answers with this version or a later one.
const RelayVersion = 3

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
	// LineNodeLineLimit: the node runs MaxLinesPerNode lines already; the line is not started (each lab gets a fair share of the node).
	LineNodeLineLimit = "NodeLineLimit"
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
	// ConsoleNode and ConsoleNodeKey (serial role only) are the node of the console end and the public key its device access service
	// published there: the only identity whose signed open the acceptor serves for this line.
	ConsoleNode    string `json:"consoleNode,omitempty"`
	ConsoleNodeKey string `json:"consoleNodeKey,omitempty"`
	// PeerKey (owner with a peer) is the public key the device access service of the serial end's node published: its reply must carry
	// a signature of that key.
	PeerKey string `json:"peerKey,omitempty"`
	// Tenant is the tenant of the lab (the namespace when it has none): the lines of a full node are shared between the tenants.
	Tenant string `json:"tenant,omitempty"`
	// Created is the creation time of the ConsoleCable (Unix seconds): with the namespace and name it orders the lines when the node is
	// full, so the same set always keeps the same lines.
	Created int64 `json:"created,omitempty"`
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
	Relay int `json:"relay"`
	// RelayKey is the public key of this device access process (Ed25519, base64url without padding): the node-agent publishes it on its
	// Node, and the peers verify the signed messages of this process with it. A new process has a new key.
	RelayKey string      `json:"relayKey,omitempty"`
	Lines    []LineState `json:"lines"`
	Error    string      `json:"error,omitempty"`
}

// PortRef names a port of a device in a CableOpen.
type PortRef struct {
	Device string `json:"device"`
	Port   string `json:"port"`
}

// CableOpen is the first message of a cable stream. Every field but Sig is covered by Sig (see the deviceaccess package, cable_auth.go).
type CableOpen struct {
	V int `json:"v"`
	// Node is the node of the dialer; its node key signs the request, and the acceptor serves it only from the console end's node.
	Node      string  `json:"node"`
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
	// Eph is the dialer's ephemeral X25519 public key; Sig its Ed25519 signature over every other field (base64url without padding).
	Eph string `json:"eph"`
	Sig string `json:"sig"`
}

// CableReply answers a CableOpen. Nonce repeats the nonce of the request (empty when it was malformed), Accept is the acceptor's nonce
// and Eph its ephemeral X25519 key. Sig is the acceptor's signature over the digest of the open and every field of the reply; it is set
// only once the dialer was authenticated, so a caller without a valid open never gets anything signed.
type CableReply struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Nonce  string `json:"nonce"`
	Accept string `json:"accept,omitempty"`
	Eph    string `json:"eph,omitempty"`
	Sig    string `json:"sig,omitempty"`
}

// CodePortDown is the refusal of a cable stream whose serial port cannot be attached now (busy, no keeper, the port failed).
const CodePortDown = "port-down"
