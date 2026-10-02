package demux

import (
	"encoding/base64"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// udp is a local UDP socket of the test.
func udp(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func recv(c *net.UDPConn, wait time.Duration) []byte {
	_ = c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 2048)
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

// initPacket is a 148-byte handshake initiation with a valid mac1 for the group key.
func initPacket(key Mac1Key, ci uint32) []byte {
	p := make([]byte, 148)
	p[0] = 1
	binary.LittleEndian.PutUint32(p[4:8], ci)
	mac := computeMAC(key[:], p[:116])
	copy(p[116:132], mac[:])
	return p
}

func typed(typ byte, a, b uint32, size int) []byte {
	p := make([]byte, size)
	p[0] = typ
	binary.LittleEndian.PutUint32(p[4:8], a)
	if size >= 12 {
		binary.LittleEndian.PutUint32(p[8:12], b)
	}
	return p
}

type rig struct {
	demux   *Demux
	backend *net.UDPConn // the VPN pod (its Service address)
	client  *net.UDPConn
	key     Mac1Key
	stop    chan struct{}
}

func newRig(t *testing.T, l Limits) *rig {
	t.Helper()
	backend, client := udp(t), udp(t)
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i + 7)
	}
	key, err := computeMac1Key(base64.StdEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	table := NewTable()
	if err := table.Update("g1", base64.StdEncoding.EncodeToString(pub), backend.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	ct := NewConnTrackWithLimits(l)
	d, err := NewWithLimits("127.0.0.1:0", table, ct, l)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go d.Run(stop)
	t.Cleanup(func() { close(stop); d.Close() })
	return &rig{demux: d, backend: backend, client: client, key: key, stop: stop}
}

func (r *rig) toDemux(c *net.UDPConn, pkt []byte) {
	_, _ = c.WriteToUDP(pkt, r.demux.LocalAddr().(*net.UDPAddr))
}

// handshake runs a full handshake: the client's index ci, the server's si.
func (r *rig) handshake(t *testing.T, ci, si uint32) {
	t.Helper()
	r.toDemux(r.client, initPacket(r.key, ci))
	if recv(r.backend, 2*time.Second) == nil {
		t.Fatal("the init did not reach the VPN pod")
	}
	r.toDemux(r.backend, typed(2, si, ci, 92))
	if got := recv(r.client, 2*time.Second); got == nil || got[0] != 2 {
		t.Fatalf("the answer did not reach the client: %v", got)
	}
}

func TestHandshakeAndTransportThroughTheDemux(t *testing.T) {
	r := newRig(t, DefaultLimits())
	r.handshake(t, 0x11, 0x22)
	// client to server carries the server's index, server to client the client's
	r.toDemux(r.client, typed(4, 0x22, 0, 64))
	if got := recv(r.backend, 2*time.Second); got == nil || got[0] != 4 {
		t.Fatal("client transport did not reach the VPN pod")
	}
	r.toDemux(r.backend, typed(4, 0x11, 0, 64))
	if got := recv(r.client, 2*time.Second); got == nil || got[0] != 4 {
		t.Fatal("server transport did not reach the client")
	}
}

// Only the VPN pod the init went to may answer it: a type 2 from anywhere else is a forgery and is dropped.
func TestType2OnlyFromTheVPNPod(t *testing.T) {
	r := newRig(t, DefaultLimits())
	attacker := udp(t)
	r.toDemux(r.client, initPacket(r.key, 0x11))
	recv(r.backend, 2*time.Second)
	r.toDemux(attacker, typed(2, 0x66, 0x11, 92))
	if got := recv(r.client, 400*time.Millisecond); got != nil {
		t.Fatal("a type 2 from another address must not reach the client")
	}
	if _, ok := r.demux.conntrack.LookupSender(0x66); ok {
		t.Fatal("and must not create a session")
	}
	r.toDemux(r.backend, typed(2, 0x22, 0x11, 92))
	if got := recv(r.client, 2*time.Second); got == nil {
		t.Fatal("the real answer passes")
	}
}

// Cookie replies are forwarded, from the exact expected source only.
func TestType3IsForwardedFromTheExpectedSourceOnly(t *testing.T) {
	r := newRig(t, DefaultLimits())
	attacker := udp(t)
	r.toDemux(r.client, initPacket(r.key, 0x11))
	recv(r.backend, 2*time.Second)
	// the VPN pod, under load, answers the init with a cookie reply addressed to the client's index
	r.toDemux(attacker, typed(3, 0x11, 0, 64))
	if got := recv(r.client, 400*time.Millisecond); got != nil {
		t.Fatal("a forged cookie reply must not be forwarded")
	}
	r.toDemux(r.backend, typed(3, 0x11, 0, 64))
	if got := recv(r.client, 2*time.Second); got == nil || got[0] != 3 {
		t.Fatal("the cookie reply of the VPN pod is forwarded")
	}
}

// A source that hammers handshake inits runs out of its budget; another source is not affected.
func TestHandshakeRateLimitPerSource(t *testing.T) {
	l := DefaultLimits()
	l.HandshakeBurst, l.HandshakeRate = 3, 0.5
	r := newRig(t, l)
	for i := uint32(0); i < 10; i++ {
		r.toDemux(r.client, initPacket(r.key, 100+i))
	}
	got := 0
	for recv(r.backend, 400*time.Millisecond) != nil {
		got++
	}
	if got != 3 {
		t.Fatalf("the backend saw %d inits, want the burst of 3", got)
	}
	// another source address has its own budget (127.0.0.2 on linux only: use the limiter directly)
	if !r.demux.handshakes.allow("203.0.113.9") {
		t.Fatal("another source has its own budget")
	}
}

// Guessing session indexes from one source is rate-limited: past the budget even a correct guess is not forwarded.
func TestGuessingIndexesIsRateLimited(t *testing.T) {
	l := DefaultLimits()
	l.MissBurst, l.MissRate = 5, 0.1
	r := newRig(t, l)
	r.handshake(t, 0x11, 0x22)
	attacker := udp(t)
	for i := uint32(0); i < 20; i++ { // wrong guesses spend the budget
		r.toDemux(attacker, typed(4, 0x1000+i, 0, 64))
	}
	time.Sleep(100 * time.Millisecond)
	r.toDemux(attacker, typed(4, 0x22, 0, 64)) // the right index, from the wrong place
	if got := recv(r.backend, 400*time.Millisecond); got != nil {
		t.Fatal("a source that guessed past its budget must not hijack the session")
	}
	// the real client is unaffected: its own packets match its session
	r.toDemux(r.client, typed(4, 0x22, 0, 64))
	if got := recv(r.backend, 2*time.Second); got == nil {
		t.Fatal("the client's own transport still flows")
	}
}

func TestOnlyNormalPacketsFlowAndTinyOnesAreIgnored(t *testing.T) {
	r := newRig(t, DefaultLimits())
	for _, p := range [][]byte{{}, {1}, {4, 0, 0}, {2, 0, 0, 0, 1, 2, 3}, {3, 0, 0, 0}, {9, 9, 9, 9, 9, 9, 9, 9}} {
		r.toDemux(r.client, p) // must not panic or forward
	}
	if got := recv(r.backend, 300*time.Millisecond); got != nil {
		t.Fatal("garbage must not be forwarded")
	}
}
