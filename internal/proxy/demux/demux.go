package demux

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Demux handles incoming WireGuard UDP packets on the one public port every team shares.
//
//	type 1 (handshake init)   forwarded to the group's VPN pod found by mac1; rate-limited (all sources together)
//	type 2 (handshake answer) accepted only from the backend the init was forwarded to
//	type 3 (cookie reply)     forwarded like a transport packet, but only from the exact expected source
//	type 4 (transport data)   forwarded by receiver index; roaming allowed at most once per interval
//
// Nothing here is authenticated (the demux does not hold the keys: the VPN pods do, and WireGuard's own handshake is
// the boundary), so everything a stranger can make it hold or spend is capped (see Limits) and nothing is logged per
// packet.
type Demux struct {
	table     *Table
	conntrack *ConnTrack
	conn      *net.UDPConn
	// There is no limit per source address: one NAT hides a whole event, and behind a load balancer every client may share an address.
	global    *limiter // type 1, all sources together
	strangers *limiter // packets that match no session or come from an unexpected source, all sources together
	readers   int
}

func New(listenAddr string, table *Table, ct *ConnTrack) (*Demux, error) {
	return NewWithLimits(listenAddr, table, ct, ct.limits)
}

func NewWithLimits(listenAddr string, table *Table, ct *ConnTrack, l Limits) (*Demux, error) {
	addr, err := net.ResolveUDPAddr("udp4", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve addr %s: %w", listenAddr, err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen UDP %s: %w", listenAddr, err)
	}
	return &Demux{
		table: table, conntrack: ct, conn: conn,
		global:    newLimiter(l.GlobalHandshakeRate, l.GlobalHandshakeBurst),
		strangers: newLimiter(l.MissRate, l.MissBurst),
		readers:   max(l.Readers, 1),
	}, nil
}

// LocalAddr is the address the demux listens on.
func (d *Demux) LocalAddr() net.Addr { return d.conn.LocalAddr() }

// Run processes incoming WireGuard packets until stop is closed. Several goroutines read the socket (a UDP socket is safe to read
// from many), so the cost of one packet does not hold the others up.
func (d *Demux) Run(stop <-chan struct{}) {
	var wg sync.WaitGroup
	for i := 0; i < d.readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.read(stop)
		}()
	}
	wg.Wait()
}

func (d *Demux) read(stop <-chan struct{}) {
	// A WireGuard datagram is at most 1500 bytes or so; anything the buffer cannot hold is not one.
	buf := make([]byte, 2048)
	for {
		select {
		case <-stop:
			_ = d.conn.Close()
			return
		default:
		}
		n, src, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			// On stop, conn is closed and ReadFromUDP returns an error.
			select {
			case <-stop:
				return
			default:
				continue
			}
		}
		d.handle(buf[:n], src)
	}
}

// The exact sizes of the fixed WireGuard messages: a handshake initiation is 148 bytes, a response 92, a cookie reply 64, and a
// transport packet at least 32. Anything else is not WireGuard and is dropped before it costs anything.
const (
	sizeInit      = 148
	sizeResponse  = 92
	sizeCookie    = 64
	sizeTransport = 32
)

// handle dispatches one datagram.
func (d *Demux) handle(pkt []byte, src *net.UDPAddr) {
	// Whatever a stranger sends must never stop the demux for everyone: a panic on one packet loses that packet only.
	defer func() {
		if r := recover(); r != nil {
			ctrl.Log.WithName("demux").Info("dropped a packet that panicked the handler", "panic", fmt.Sprint(r))
		}
	}()
	if len(pkt) < 1 {
		return
	}
	switch pkt[0] {
	case 1:
		if len(pkt) == sizeInit {
			d.handleType1(pkt, src)
		}
	case 2:
		if len(pkt) == sizeResponse {
			d.handleType2(pkt, src)
		}
	case 3:
		if len(pkt) == sizeCookie {
			d.handleType3(pkt, src)
		}
	case 4:
		if len(pkt) >= sizeTransport {
			d.handleType4Userspace(pkt, src)
		}
	}
}

func socketOf(a *net.UDPAddr) Socket { return Socket{IP: a.IP, Port: uint16(a.Port)} }

func (d *Demux) handleType1(pkt []byte, src *net.UDPAddr) {
	log := ctrl.Log.WithName("demux")
	if len(pkt) < 8 {
		return
	}
	// A handshake init costs a scan of every group's mac1 key: all sources together pay for it from one budget.
	if !d.global.allow() {
		return
	}
	_, backend, found := d.table.FindByMac1(pkt)
	if !found {
		return
	}
	if backend == nil {
		log.V(1).Info("backend not resolved yet, dropping handshake")
		return
	}
	resolved := backend
	ci := binary.LittleEndian.Uint32(pkt[4:8])

	// Reserve Ci before forwarding — spec §4 says collision with another live session is fatal for this handshake;
	// let the client retry with a new index instead of clobbering an unrelated peer. The reservation also refuses a
	// handshake that would pass the table's caps.
	if !d.conntrack.AddPartial(ci, socketOf(src), socketOf(resolved)) {
		return
	}

	// Send via main listen socket so backend's type-2 response returns to it,
	// not to an ephemeral socket that would be closed before the reply arrives.
	if _, err := d.conn.WriteToUDP(pkt, resolved); err != nil {
		log.V(1).Info("forward type1 failed", "error", err.Error())
	}
}

func (d *Demux) handleType2(pkt []byte, src *net.UDPAddr) {
	if len(pkt) < 12 {
		return
	}
	si := binary.LittleEndian.Uint32(pkt[4:8])
	ci := binary.LittleEndian.Uint32(pkt[8:12])

	clientDst, backend, found := d.conntrack.LookupPartial(ci)
	if !found {
		return
	}
	// The answer must come from the VPN pod the init went to; anyone else's type 2 is a forgery.
	if !sameSocket(backend, socketOf(src)) {
		return
	}

	// Reserve Si before forwarding to the client — if another live session owns
	// this index, drop the response so upstream WG retries with a fresh index.
	if !d.conntrack.Complete(si, ci, clientDst, socketOf(src)) {
		return
	}

	dst := &net.UDPAddr{IP: clientDst.IP, Port: int(clientDst.Port)}
	_, _ = d.conn.WriteToUDP(pkt, dst)
}

// handleType3 forwards a cookie reply (a peer under load answers a handshake with one): to the session its receiver
// index names, and only from the exact address that session expects.
func (d *Demux) handleType3(pkt []byte, src *net.UDPAddr) {
	if len(pkt) < 8 {
		return
	}
	receiverIndex := binary.LittleEndian.Uint32(pkt[4:8])
	if _, ok := d.conntrack.Expected(receiverIndex); !ok {
		return
	}
	dst, found := d.conntrack.LookupStrict(receiverIndex, socketOf(src))
	if !found {
		return
	}
	_, _ = d.conn.WriteToUDP(pkt, &net.UDPAddr{IP: dst.IP, Port: int(dst.Port)})
}

func (d *Demux) handleType4Userspace(pkt []byte, src *net.UDPAddr) {
	if len(pkt) < 8 {
		return
	}
	receiverIndex := binary.LittleEndian.Uint32(pkt[4:8])
	from := socketOf(src)
	// A packet that matches no session, or comes from other than the session's address (a guess, a spoof, or a roam),
	// is paid for from one budget for all sources: guessing the 32-bit index is rate-limited.
	if expected, ok := d.conntrack.Expected(receiverIndex); !ok || !sameSocket(expected, from) {
		if !d.strangers.allow() {
			return
		}
	}
	dst, found := d.conntrack.LookupForward(receiverIndex, from)
	if !found {
		return
	}
	_, _ = d.conn.WriteToUDP(pkt, &net.UDPAddr{IP: dst.IP, Port: int(dst.Port)})
}

func (d *Demux) Close() {
	_ = d.conn.Close()
}
