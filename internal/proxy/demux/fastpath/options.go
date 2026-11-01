package fastpath

import (
	"time"

	"github.com/cybericebox/laboratory/internal/proxy/demux"
)

// Options configure the fast path of one demux.
type Options struct {
	Mode    demux.Mode // tc or xdp
	PinPath string     // where maps and links are pinned (a directory on a bpffs)
	// Interfaces are the external devices (the clients' packets arrive on them, replies leave through the first). Empty = the
	// device that holds HostIP.
	Interfaces []string
	HostIP     string
	// SourcePort is the demux socket's port: the source port of the requests the fast path sends towards a server.
	SourcePort uint16
	MaxEntries int           // capacity of the session map (two entries per session); 0 = the object's default
	TTL        time.Duration // a session idle for longer is left to the slow path
	Rate       float64       // per-session packets per second, both directions together; 0 = unlimited
	Burst      int
	// NoByteLimiter loads the programs without the byte limiter, as after a refusal of the verifier (an escape hatch, and how the tests
	// run the fallback).
	NoByteLimiter bool
}

// Stats are the fast path's counters summed over the CPUs, by the names of demux.FastPathResults.
type Stats map[string]uint64

// ReplyBaseDevice is where the replies of servers on other nodes arrive (after Cilium's overlay processing); the replies of
// a same-node server arrive on its host-side veth, which the fast path learns from the session.
const ReplyBaseDevice = "cilium_host"

// DefaultPinPath is the default of DEMUX_BPF_PIN_PATH (the config test holds the two together).
const DefaultPinPath = "/sys/fs/bpf/cybericebox/wg-demux"
