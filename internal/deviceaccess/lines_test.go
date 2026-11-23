package deviceaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-logr/logr"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/portkeeper"
)

// A console line in the fake runtime: r1 has console0 (its console program answers every byte in capitals), pc1 has serial0 (what the
// test writes in it is what a program in pc1 writes to /dev/ttyCON0; what reaches it is what that program reads).

var (
	r1Ports  = portkeeper.Spec{Console: []portkeeper.ConsolePort{{Name: "console0"}}}
	pc1Ports = portkeeper.Spec{Serial: []portkeeper.SerialPort{{Name: "serial0", Path: "/dev/ttyCON0"}}}
)

func portSandbox(id, uid, device, labUID string, ports portkeeper.Spec) *fakeContainer {
	l := map[string]string{
		labelKind: kindSandbox, labelPodNamespace: testNS, labelPodUID: uid,
		names.LabelLab: testLab, names.LabelDevice: device, names.LabelLabUID: labUID, names.LabelDeviceUID: "dev-" + device,
		names.LabelPorts: portkeeper.Hash(ports),
	}
	return &fakeContainer{ContainerInfo: ContainerInfo{ID: id, Labels: l}, running: true}
}

// serialTerm is the program in pc1 that uses the serial device.
type serialTerm struct {
	mu  sync.Mutex
	got bytes.Buffer
	in  chan []byte
}

func newSerialTerm() *serialTerm { return &serialTerm{in: make(chan []byte, 16)} }

func (s *serialTerm) received() string { s.mu.Lock(); defer s.mu.Unlock(); return s.got.String() }

// attachBehaviour emulates probe port-attach: busy[container] > 0 makes the next attaches end with ExitBusy.
type attachBehaviour struct {
	mu     sync.Mutex
	busy   map[string]int
	serial map[string]*serialTerm
}

func (a *attachBehaviour) run(proc *specs.Process, stdio IO, ex *fakeExec) {
	if len(proc.Args) != 3 || proc.Args[0] != names.ProbeBinary || proc.Args[1] != portkeeper.CommandAttach || proc.User.UID != 0 || proc.Terminal {
		ex.exit(99)
		return
	}
	a.mu.Lock()
	if a.busy[ex.container] > 0 {
		a.busy[ex.container]--
		a.mu.Unlock()
		ex.exit(portkeeper.ExitBusy)
		return
	}
	term := a.serial[ex.container]
	a.mu.Unlock()
	if _, err := stdio.Stdout.Write([]byte{portkeeper.AttachOK}); err != nil {
		return
	}
	if term == nil { // a console program: answer in capitals
		echo(proc, stdio, ex)
		return
	}
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stdio.Stdin.Read(buf)
			term.mu.Lock()
			term.got.Write(buf[:n])
			term.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case b := <-term.in:
			if _, err := stdio.Stdout.Write(b); err != nil {
				return
			}
		case <-ex.killed:
			return
		}
	}
}

func lineRuntime(t *testing.T, withR1, withPC1 bool) (*fakeRuntime, *attachBehaviour, *serialTerm) {
	t.Helper()
	rt := &fakeRuntime{}
	term := newSerialTerm()
	ab := &attachBehaviour{busy: map[string]int{}, serial: map[string]*serialTerm{"pc1-c": term}}
	if withR1 {
		rt.add(portSandbox("r1-sb", "r1-pod", "r1", "lab-uid", r1Ports))
		rt.add(&fakeContainer{ContainerInfo: ContainerInfo{ID: "r1-c", Labels: appLabels("r1-pod", "r1")}, running: true, spec: deviceSpec()})
	}
	if withPC1 {
		rt.add(portSandbox("pc1-sb", "pc1-pod", "pc1", "lab-uid", pc1Ports))
		rt.add(&fakeContainer{ContainerInfo: ContainerInfo{ID: "pc1-c", Labels: appLabels("pc1-pod", "pc1")}, running: true, spec: deviceSpec()})
	}
	rt.behaviour = ab.run
	return rt, ab, term
}

// testIDs are the identities of the device access services of the test nodes.
var testIDs = func() map[string]*Identity {
	out := map[string]*Identity{}
	for _, n := range []string{"n1", "n2", "n3"} {
		id, err := NewIdentity()
		if err != nil {
			panic(err)
		}
		out[n] = id
	}
	return out
}()

// lineSpec is the line of the tests: the console end on n1, the serial end on n2 when peer is set; the keys as the node-agents publish them.
func lineSpec(role, peer string) wire.LineSpec {
	l := wire.LineSpec{Namespace: testNS, Name: "cable-1", UID: "cable-uid", Lab: testLab, LabUID: "lab-uid", Role: role, Peer: peer,
		Serial:  wire.LineEnd{Device: "pc1", Port: "serial0", Ports: portkeeper.Marshal(pc1Ports)},
		Console: wire.LineEnd{Device: "r1", Port: "console0", Ports: portkeeper.Marshal(r1Ports)}}
	if role == wire.RoleSerial {
		l.ConsoleNode, l.ConsoleNodeKey = "n1", testIDs["n1"].PublicKey()
	}
	if peer != "" {
		l.PeerKey = testIDs["n2"].PublicKey()
	}
	return l
}

func lineService(t *testing.T, rt Runtime) *Service { return lineServiceOn(t, rt, "n1") }

// lineServiceOn is the service of a node, with the identity of that node (or the given one).
func lineServiceOn(t *testing.T, rt Runtime, node string, id ...*Identity) *Service {
	t.Helper()
	lim := DefaultLimits()
	lim.WriteTimeout, lim.Cleanup = 2*time.Second, time.Second
	identity := testIDs[node]
	if len(id) > 0 {
		identity = id[0]
	}
	s := New(Config{Runtime: rt, Limits: lim, Log: logr.Discard(), Identity: identity, NodeName: node})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

func fastRetry(t *testing.T) {
	t.Helper()
	oldMin, oldMax := lineRetryMin, lineRetryMax
	lineRetryMin, lineRetryMax = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { lineRetryMin, lineRetryMax = oldMin, oldMax })
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stateOf(s *Service, name string) wire.LineState {
	for _, st := range s.LineStates().Lines {
		if st.Name == name {
			return st
		}
	}
	return wire.LineState{}
}

// Both ends on this node: the owner attaches both ports with probe port-attach and copies the bytes in process. What pc1 writes on its
// serial device reaches r1's console program and r1's answer comes back.
func TestLineSameNode(t *testing.T) {
	fastRetry(t)
	rt, _, term := lineRuntime(t, true, true)
	s := lineService(t, rt)
	s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, "")}})
	waitFor(t, "the line up", func() bool { return stateOf(s, "cable-1").Up })
	term.in <- []byte("show version\r")
	waitFor(t, "the answer of the console", func() bool { return term.received() == "SHOW VERSION\r" })
	st := stateOf(s, "cable-1")
	if !st.ConsoleAttached || !st.SerialAttached || st.Reason != "" || st.Role != wire.RoleOwner {
		t.Fatalf("state %+v", st)
	}
	// the same set again keeps the line (a node-agent restart), no new attach
	n := len(rt.started())
	s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, "")}})
	time.Sleep(100 * time.Millisecond)
	if len(rt.started()) != n || !stateOf(s, "cable-1").Up {
		t.Fatal("an unchanged line must keep running")
	}
	// a set without it stops it and removes the execs
	s.SyncLines(wire.LinesRequest{})
	waitFor(t, "the execs removed", rt.allClosed)
	if len(s.LineStates().Lines) != 0 {
		t.Fatal("the line is still listed")
	}
}

// An end restarts (the device restarted: its attach ends): the line goes down and the owner attaches again.
func TestLineReattachesAfterAnEndEnds(t *testing.T) {
	fastRetry(t)
	rt, _, term := lineRuntime(t, true, true)
	s := lineService(t, rt)
	s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, "")}})
	waitFor(t, "the line up", func() bool { return stateOf(s, "cable-1").Up })
	for _, ex := range rt.started() {
		if ex.container == "r1-c" {
			_ = ex.Close(context.Background())
		}
	}
	waitFor(t, "a new attach of r1", func() bool {
		n := 0
		for _, ex := range rt.started() {
			if ex.container == "r1-c" {
				n++
			}
		}
		return n >= 2 && stateOf(s, "cable-1").Up
	})
	term.in <- []byte("x")
	waitFor(t, "bytes after the reattach", func() bool { return strings.HasSuffix(term.received(), "X") })
}

// The checks before an attach: a port list that is not the one stamped on the pod, a pod of another lab with the same names, a busy
// port, a missing device. Nothing is run in a device that fails them.
func TestLineRefusals(t *testing.T) {
	fastRetry(t)
	t.Run("ports not the device's", func(t *testing.T) {
		rt, _, _ := lineRuntime(t, true, true)
		s := lineService(t, rt)
		spec := lineSpec(wire.RoleOwner, "")
		spec.Console.Ports = portkeeper.Marshal(portkeeper.Spec{Console: []portkeeper.ConsolePort{{Name: "console0"}, {Name: "aux0"}}})
		s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{spec}})
		waitFor(t, "PortDown", func() bool { return stateOf(s, "cable-1").Reason == wire.LinePortDown })
		time.Sleep(100 * time.Millisecond)
		if len(rt.started()) != 0 {
			t.Fatal("an exec was started for a port list that is not the device's")
		}
	})
	t.Run("a port of the other kind", func(t *testing.T) {
		rt, _, _ := lineRuntime(t, true, true)
		s := lineService(t, rt)
		spec := lineSpec(wire.RoleOwner, "")
		spec.Console.Ports = spec.Serial.Ports // the list has no console0
		spec.Console.Device = "pc1"
		spec.Serial.Device = "r1"
		s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{spec}})
		waitFor(t, "PortDown", func() bool { return stateOf(s, "cable-1").Reason == wire.LinePortDown })
		if len(rt.started()) != 0 {
			t.Fatal("an exec was started")
		}
	})
	t.Run("another lab with the same names", func(t *testing.T) {
		rt, _, _ := lineRuntime(t, true, true)
		s := lineService(t, rt)
		spec := lineSpec(wire.RoleOwner, "")
		spec.LabUID = "other-lab-uid"
		s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{spec}})
		waitFor(t, "WaitingForDevice", func() bool { return stateOf(s, "cable-1").Reason == wire.LineWaitingForDevice })
		time.Sleep(100 * time.Millisecond)
		if len(rt.started()) != 0 {
			t.Fatal("an exec was started in a device of another lab")
		}
	})
	t.Run("no device yet", func(t *testing.T) {
		rt, _, _ := lineRuntime(t, false, true)
		s := lineService(t, rt)
		s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, "")}})
		time.Sleep(100 * time.Millisecond)
		if st := stateOf(s, "cable-1"); st.Reason != wire.LineWaitingForDevice || st.Up {
			t.Fatalf("state %+v", st)
		}
	})
	t.Run("busy, then free", func(t *testing.T) {
		rt, ab, _ := lineRuntime(t, true, true)
		ab.busy["r1-c"] = 3
		s := lineService(t, rt)
		s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, "")}})
		waitFor(t, "PortDown", func() bool { return stateOf(s, "cable-1").Reason == wire.LinePortDown })
		waitFor(t, "the line up after the retries", func() bool { return stateOf(s, "cable-1").Up })
	})
	t.Run("invalid lines", func(t *testing.T) {
		rt, _, _ := lineRuntime(t, true, true)
		s := lineService(t, rt)
		bad := []wire.LineSpec{lineSpec("admin", ""), lineSpec(wire.RoleSerial, "10.0.0.1:1"), lineSpec(wire.RoleOwner, "no-port")}
		swapped := lineSpec(wire.RoleOwner, "")
		swapped.Serial.Port, swapped.Console.Port = "console0", "serial0"
		bad = append(bad, swapped)
		for i := range bad {
			bad[i].Name = "bad" + string(rune('a'+i))
		}
		if got := s.SyncLines(wire.LinesRequest{Lines: bad}); len(got.Lines) != 0 {
			t.Fatalf("invalid lines were taken: %+v", got.Lines)
		}
	})
}

// Ends on two nodes: the owner dials the service of the serial end's node over the cable stream; the bytes go both ways.
func TestLineCrossNode(t *testing.T) {
	fastRetry(t)
	rtA, _, _ := lineRuntime(t, true, false)
	rtB, _, term := lineRuntime(t, false, true)
	a, b := lineServiceOn(t, rtA, "n1"), lineServiceOn(t, rtB, "n2")
	srv := httptest.NewServer(b.CableHandler())
	t.Cleanup(srv.Close)
	peer := strings.TrimPrefix(srv.URL, "http://")

	// the owner dials before the serial node knows the line: refused, the owner waits
	a.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, peer)}})
	waitFor(t, "WaitingForDevice", func() bool { return stateOf(a, "cable-1").Reason == wire.LineWaitingForDevice })
	b.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleSerial, "")}})
	waitFor(t, "the line up on both nodes", func() bool { return stateOf(a, "cable-1").Up && stateOf(b, "cable-1").Up })
	term.in <- []byte("hostname\r")
	waitFor(t, "the answer of the console over the stream", func() bool { return term.received() == "HOSTNAME\r" })

	// the serial node drops the line: the owner sees the stream end, and the line comes back when it is given again
	b.SyncLines(wire.LinesRequest{})
	waitFor(t, "the owner down", func() bool { return !stateOf(a, "cable-1").Up })
	b.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleSerial, "")}})
	waitFor(t, "the line up again", func() bool { return stateOf(a, "cable-1").Up })
}

// The keys decide: a peer whose identity is not the one its node published is unreachable for the owner, and an owner whose identity is not
// the one the console end's node published is refused by the acceptor. Nothing is attached in either case.
func TestLineCrossNodeWrongKey(t *testing.T) {
	fastRetry(t)
	impostor, _ := NewIdentity()
	t.Run("the acceptor is not who its node published", func(t *testing.T) {
		rtA, _, _ := lineRuntime(t, true, false)
		rtB, _, _ := lineRuntime(t, false, true)
		a := lineServiceOn(t, rtA, "n1")
		b := lineServiceOn(t, rtB, "n2", impostor)
		srv := httptest.NewServer(b.CableHandler())
		t.Cleanup(srv.Close)
		b.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleSerial, "")}})
		a.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, strings.TrimPrefix(srv.URL, "http://"))}})
		waitFor(t, "PeerUnreachable", func() bool { return stateOf(a, "cable-1").Reason == wire.LinePeerUnreachable })
	})
	t.Run("the dialer is not who the console end's node published", func(t *testing.T) {
		rtA, _, _ := lineRuntime(t, true, false)
		rtB, _, _ := lineRuntime(t, false, true)
		a := lineServiceOn(t, rtA, "n1", impostor)
		b := lineServiceOn(t, rtB, "n2")
		srv := httptest.NewServer(b.CableHandler())
		t.Cleanup(srv.Close)
		b.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleSerial, "")}})
		a.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, strings.TrimPrefix(srv.URL, "http://"))}})
		waitFor(t, "PeerUnreachable", func() bool { return stateOf(a, "cable-1").Reason == wire.LinePeerUnreachable })
		if len(rtB.started()) != 0 {
			t.Fatal("the serial port was attached for an open the console end's node did not sign")
		}
	})
}

// dialCable opens a raw WebSocket to the cable endpoint (what any pod of the cluster could do).
func dialCable(t *testing.T, srv *httptest.Server) (*websocket.Conn, context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+wire.CablePath, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c.SetReadLimit(cableReadLimit)
	return c, ctx, cancel
}

func sendOpen(t *testing.T, srv *httptest.Server, open any) (wire.CableReply, *websocket.Conn, context.Context) {
	t.Helper()
	c, ctx, cancel := dialCable(t, srv)
	t.Cleanup(func() { _ = c.CloseNow(); cancel() })
	b, _ := json.Marshal(open)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var r wire.CableReply
	_ = json.Unmarshal(msg, &r)
	return r, c, ctx
}

// What another node can do on the acceptor: nothing. A request for another lab, another cable UID, another port or namespace, from a node
// that is not the console end's, signed by a node that claims to be the console end's (a compromised node impersonating another), unsigned,
// replayed, or signed before the acceptor started is refused, and nothing is run in the device. A refusal before authentication carries no
// signature, and a nonce that is not a nonce is never echoed.
func TestCableStreamRefusals(t *testing.T) {
	rt, _, _ := lineRuntime(t, false, true)
	s := lineServiceOn(t, rt, "n2")
	s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleSerial, "")}})
	srv := httptest.NewServer(s.CableHandler())
	t.Cleanup(srv.Close)
	base := cableOpenOf(lineSpec(wire.RoleOwner, ""))
	for name, c := range map[string]struct {
		mod  func(*wire.CableOpen)
		id   string
		node string
		want string
	}{
		"another lab":                 {func(r *wire.CableOpen) { r.Lab = "lab2" }, "n1", "n1", wire.CodeNotFound},
		"another lab uid":             {func(r *wire.CableOpen) { r.LabUID = "x" }, "n1", "n1", wire.CodeNotFound},
		"another cable":               {func(r *wire.CableOpen) { r.UID = "x" }, "n1", "n1", wire.CodeNotFound},
		"another port":                {func(r *wire.CableOpen) { r.Serial.Port = "serial1" }, "n1", "n1", wire.CodeNotFound},
		"another namespace":           {func(r *wire.CableOpen) { r.Namespace = "lab-group-b" }, "n1", "n1", wire.CodeNotFound},
		"not the console end's node":  {func(*wire.CableOpen) {}, "n3", "n3", wire.CodeNotFound},
		"n3 claiming to be n1":        {func(*wire.CableOpen) {}, "n3", "n1", wire.CodeUnauthorized},
		"the serial node claiming n1": {func(*wire.CableOpen) {}, "n2", "n1", wire.CodeUnauthorized},
	} {
		open := base
		c.mod(&open)
		signed, _, _ := signOpen(testIDs[c.id], c.node, open, time.Now())
		if r, _, _ := sendOpen(t, srv, signed); r.OK || r.Error != c.want || r.Sig != "" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	tampered, _, _ := signOpen(testIDs["n1"], "n1", base, time.Now())
	tampered.Serial.Device = "r1"
	if r, _, _ := sendOpen(t, srv, tampered); r.OK || r.Sig != "" {
		t.Errorf("a field changed after signing: %+v", r)
	}
	unsigned, _, _ := signOpen(testIDs["n1"], "n1", base, time.Now())
	unsigned.Sig = strings.Repeat("A", 86)
	if r, _, _ := sendOpen(t, srv, unsigned); r.OK || r.Error != wire.CodeUnauthorized || r.Sig != "" || r.Nonce != unsigned.Nonce {
		t.Errorf("unsigned: %+v", r)
	}
	odd := unsigned
	odd.Nonce = "<script>" + strings.Repeat("a", 24)
	if r, _, _ := sendOpen(t, srv, odd); r.OK || r.Nonce != "" || r.Sig != "" {
		t.Errorf("a malformed nonce: %+v", r)
	}
	// signed before this process started: it may be a replay of an open the previous process accepted
	early, _, _ := signOpen(testIDs["n1"], "n1", base, s.cable.started.Add(-time.Second))
	if r, _, _ := sendOpen(t, srv, early); r.OK || r.Error != wire.CodeUnauthorized {
		t.Errorf("an open from before the start: %+v", r)
	}
	if len(rt.started()) != 0 {
		t.Fatal("a refused request attached the serial port")
	}
	good, _, _ := signOpen(testIDs["n1"], "n1", base, time.Now())
	if r, _, _ := sendOpen(t, srv, good); !r.OK || checkReply(testIDs["n2"].PublicKey(), openTranscript(good), r, good.Nonce) != nil {
		t.Fatalf("the real line: %+v", r)
	}
	if r, _, _ := sendOpen(t, srv, good); r.OK || r.Error != wire.CodeUnauthorized {
		t.Fatalf("a replayed request: %+v", r)
	}
}

// A signed open replayed by somebody on the path whose original never reached the acceptor is accepted once, but the replayer cannot
// use the stream: the session comes from the dialer's ephemeral X25519 key, whose private half the replayer does not have. What the
// acceptor sends is sealed (no byte of the serial line is readable), and a frame it did not seal ends the line. The open cannot be used a
// second time.
func TestCableStreamReplayGetsNothing(t *testing.T) {
	rt, _, term := lineRuntime(t, false, true)
	s := lineServiceOn(t, rt, "n2")
	s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleSerial, "")}})
	srv := httptest.NewServer(s.CableHandler())
	t.Cleanup(srv.Close)
	// the original open, captured and never delivered (its ephemeral key stays with the dialer)
	captured, _, _ := signOpen(testIDs["n1"], "n1", cableOpenOf(lineSpec(wire.RoleOwner, "")), time.Now())
	r, c, ctx := sendOpen(t, srv, captured)
	if !r.OK {
		t.Fatalf("the replay is answered like the original would be: %+v", r)
	}
	waitFor(t, "the serial port attached", func() bool { return stateOf(s, "cable-1").Up })
	term.in <- []byte("SECRET-ON-THE-LINE")
	_, msg, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(msg), "SECRET") {
		t.Fatal("the bytes of the line travel in the clear")
	}
	// a session from another ephemeral key (the replayer's own) does not open it
	mine, _ := newEphemeral()
	shared, _ := mine.shared(r.Eph)
	openT := openTranscript(captured)
	guess, _ := newSealedConn(nil, sessionSecret(shared, openT, replyTranscript(openT, r)), true)
	if _, err := guess.recv.Open(nil, gcmNonce(0), msg[8:], msg[:8]); err == nil {
		t.Fatal("a frame opened without the dialer's ephemeral key")
	}
	if err := c.Write(ctx, websocket.MessageBinary, wire.Data([]byte("rm -rf /\r"))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the line ended", func() bool { return !stateOf(s, "cable-1").Up })
	if strings.Contains(term.received(), "rm -rf") {
		t.Fatal("an injected frame reached the device")
	}
	if r2, _, _ := sendOpen(t, srv, captured); r2.OK || r2.Error != wire.CodeUnauthorized {
		t.Fatalf("the open used twice: %+v", r2)
	}
}

// The sealed frames: the next sequence number under the receiver's own key only; a changed, replayed, reordered or reflected frame is
// refused.
func TestSealedConn(t *testing.T) {
	key := []byte(strings.Repeat("s", 32))
	aPipe := newPipeConn()
	dialer, _ := newSealedConn(aPipe, key, true)
	acceptor, _ := newSealedConn(aPipe, key, false)
	ctx := context.Background()
	var sent [][]byte
	for _, f := range []string{"one", "two", "three"} {
		if err := dialer.writeFrame(ctx, wire.Data([]byte(f))); err != nil {
			t.Fatal(err)
		}
		m := <-aPipe.out
		sent = append(sent, m.data)
	}
	feed := func(m []byte) ([]byte, error) {
		aPipe.in <- message{false, m}
		return acceptor.readFrame(ctx)
	}
	if f, err := feed(sent[0]); err != nil || string(f[1:]) != "one" {
		t.Fatalf("first: %q %v", f, err)
	}
	if _, err := feed(sent[0]); err == nil {
		t.Fatal("a replayed frame")
	}
	acc2, _ := newSealedConn(aPipe, key, false)
	if _, err := func() ([]byte, error) { aPipe.in <- message{false, sent[2]}; return acc2.readFrame(ctx) }(); err == nil {
		t.Fatal("a frame out of order")
	}
	tampered := append([]byte(nil), sent[1]...)
	tampered[len(tampered)-1] ^= 1
	acc3, _ := newSealedConn(aPipe, key, false)
	if _, err := func() ([]byte, error) { aPipe.in <- message{false, sent[0]}; return acc3.readFrame(ctx) }(); err != nil {
		t.Fatal(err)
	}
	if _, err := func() ([]byte, error) { aPipe.in <- message{false, tampered}; return acc3.readFrame(ctx) }(); err == nil {
		t.Fatal("a changed frame")
	}
	// the dialer's own frame reflected back to it is not accepted (another key per direction)
	if _, err := func() ([]byte, error) { aPipe.in <- message{false, sent[0]}; return dialer.readFrame(ctx) }(); err == nil {
		t.Fatal("a reflected frame")
	}
	// another session (another secret) does not open it
	other, _ := newSealedConn(aPipe, []byte(strings.Repeat("o", 32)), false)
	if _, err := func() ([]byte, error) { aPipe.in <- message{false, sent[0]}; return other.readFrame(ctx) }(); err == nil {
		t.Fatal("a frame spliced from another session")
	}
}

// Handshakes that never send their first message are bounded: one address holds at most maxPendingPerSource of them, and they are closed
// after cableFirstMessage.
func TestCablePendingHandshakes(t *testing.T) {
	old := cableFirstMessage
	cableFirstMessage = 500 * time.Millisecond
	t.Cleanup(func() { cableFirstMessage = old })
	rt, _, _ := lineRuntime(t, false, true)
	s := lineServiceOn(t, rt, "n2")
	srv := httptest.NewServer(s.CableHandler())
	t.Cleanup(srv.Close)
	var held []*websocket.Conn
	for range maxPendingPerSource {
		c, _, cancel := dialCable(t, srv)
		t.Cleanup(cancel)
		held = append(held, c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+wire.CablePath, nil); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("one handshake more than the limit of a source: %v", err)
	}
	// silent handshakes are closed at the deadline, which frees the room
	for _, c := range held {
		if _, _, err := c.Read(ctx); err == nil {
			t.Fatal("a silent handshake was not closed")
		}
	}
	waitFor(t, "the room freed", func() bool {
		s.pending.mu.Lock()
		defer s.pending.mu.Unlock()
		return s.pending.total == 0
	})
}

// A data frame as large as a frame may be passes the rate cap: the relay waits for it in pieces of at most the burst.
func TestRelayTakesTheLargestFrame(t *testing.T) {
	rt, _, _ := lineRuntime(t, false, false)
	s := lineService(t, rt)
	big := bytes.Repeat([]byte("x"), wire.MaxData)
	a := &memSide{in: make(chan []byte, 1)}
	b := &memSide{in: make(chan []byte)}
	a.in <- big
	done := make(chan int, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- s.relay(ctx, a, b, nil) }()
	waitFor(t, "the frame through", func() bool { return b.got() == len(big) })
	cancel()
	<-done
}

type memSide struct {
	mu     sync.Mutex
	in     chan []byte
	n      int
	closed chan struct{}
	once   sync.Once
}

func (m *memSide) read() ([]byte, error) {
	m.once.Do(func() {
		m.mu.Lock()
		if m.closed == nil {
			m.closed = make(chan struct{})
		}
		m.mu.Unlock()
	})
	select {
	case p := <-m.in:
		return p, nil
	case <-m.closedC():
		return nil, io.EOF
	}
}

func (m *memSide) closedC() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed == nil {
		m.closed = make(chan struct{})
	}
	return m.closed
}

func (m *memSide) write(p []byte) error { m.mu.Lock(); m.n += len(p); m.mu.Unlock(); return nil }
func (m *memSide) got() int             { m.mu.Lock(); defer m.mu.Unlock(); return m.n }
func (m *memSide) close(string) {
	c := m.closedC()
	select {
	case <-c:
	default:
		close(c)
	}
}

// The limit of the node: deterministic whatever the order of the request, fair between labs, and the refused lines are answered with
// NodeLineLimit.
func TestAdmitLinesIsFairAndDeterministic(t *testing.T) {
	mk := func(lab string, i int) wire.LineSpec {
		l := lineSpec(wire.RoleOwner, "")
		l.Namespace, l.Lab, l.Name, l.Created = "ns", lab, lab+"-"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676)), int64(i)
		return l
	}
	var lines []wire.LineSpec
	for i := range 600 {
		lines = append(lines, mk("big", i))
	}
	for i := range 10 {
		lines = append(lines, mk("small", 1000+i))
	}
	admitted, refused := admitLines(lines)
	count := map[string]int{}
	for _, l := range admitted {
		count[l.Lab]++
	}
	if len(admitted) != MaxLinesPerNode || count["small"] != 10 || count["big"] != MaxLinesPerNode-10 || len(refused) != 110 {
		t.Fatalf("admitted %d %v, refused %d", len(admitted), count, len(refused))
	}
	reversed := make([]wire.LineSpec, len(lines))
	for i := range lines {
		reversed[len(lines)-1-i] = lines[i]
	}
	again, _ := admitLines(reversed)
	for i := range admitted {
		if admitted[i].Name != again[i].Name {
			t.Fatal("the admitted lines depend on the order of the request")
		}
	}
	// the oldest cables keep their lines
	for _, l := range refused {
		if l.Lab != "big" || l.Created < 490 {
			t.Fatalf("refused %+v", l)
		}
	}
}

func TestSyncLinesReportsTheNodeLimit(t *testing.T) {
	rt, _, _ := lineRuntime(t, false, false)
	s := lineService(t, rt)
	var lines []wire.LineSpec
	for i := range MaxLinesPerNode + 1 {
		l := lineSpec(wire.RoleSerial, "")
		l.Name, l.Created = "c-"+strconv.Itoa(i), int64(i)
		lines = append(lines, l)
	}
	reply := s.SyncLines(wire.LinesRequest{Lines: lines})
	limited := 0
	for _, st := range reply.Lines {
		if st.Reason == wire.LineNodeLineLimit {
			limited++
			if st.Name != "c-"+strconv.Itoa(MaxLinesPerNode) {
				t.Fatalf("the newest cable must be the refused one: %s", st.Name)
			}
		}
	}
	if len(reply.Lines) != MaxLinesPerNode+1 || limited != 1 {
		t.Fatalf("%d lines, %d limited", len(reply.Lines), limited)
	}
}

// Each direction is capped at names.ConsoleLineMaxBytesPerSecond: 128 KiB take about two seconds, not a moment.
func TestLineRateCap(t *testing.T) {
	if testing.Short() {
		t.Skip("takes two seconds")
	}
	fastRetry(t)
	rt, _, term := lineRuntime(t, true, true)
	s := lineService(t, rt)
	s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, "")}})
	waitFor(t, "the line up", func() bool { return stateOf(s, "cable-1").Up })
	payload := bytes.Repeat([]byte("a"), 2*names.ConsoleLineMaxBytesPerSecond)
	begin := time.Now()
	go func() {
		for i := 0; i < len(payload); i += 16 << 10 {
			term.in <- payload[i : i+16<<10]
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(term.received()) < len(payload) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	took := time.Since(begin)
	if len(term.received()) != len(payload) {
		t.Fatalf("got %d of %d bytes", len(term.received()), len(payload))
	}
	if took < 1500*time.Millisecond {
		t.Fatalf("%d bytes crossed in %s: the cap is not applied", len(payload), took)
	}
}

// cable.sock: PUT sets the lines and answers their state; an unknown field is refused.
func TestCableSockHandler(t *testing.T) {
	rt, _, _ := lineRuntime(t, true, true)
	s := lineService(t, rt)
	h := s.CableSockHandler()
	body, _ := json.Marshal(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, "")}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, wire.LinesPath, bytes.NewReader(body)))
	var r wire.LinesReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || w.Code != http.StatusOK || r.Relay != wire.RelayVersion || len(r.Lines) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, wire.LinesPath, strings.NewReader(`{"lines":[],"command":["/bin/sh"]}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, wire.LinesPath, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "cable-1") {
		t.Fatalf("GET: %d %s", w.Code, w.Body)
	}
	_, _ = io.Copy(io.Discard, w.Body)
}

// The share is per tenant: a tenant with many labs gets no more of a full node than a tenant with one, and inside a tenant the labs share
// its part.
func TestAdmitLinesIsFairBetweenTenants(t *testing.T) {
	var lines []wire.LineSpec
	n := 0
	add := func(tenant, lab string, count int) {
		for range count {
			l := lineSpec(wire.RoleOwner, "")
			l.Namespace, l.Lab, l.Tenant, l.Name, l.Created = "ns-"+tenant, lab, tenant, "c"+strconv.Itoa(n), int64(n)
			n++
			lines = append(lines, l)
		}
	}
	for i := range 10 {
		add("many", "lab"+strconv.Itoa(i), 100)
	}
	add("one", "only", 300)
	admitted, refused := admitLines(lines)
	byTenant, byLab := map[string]int{}, map[string]int{}
	for _, l := range admitted {
		byTenant[l.Tenant]++
		byLab[l.Lab]++
	}
	if len(admitted) != MaxLinesPerNode || len(refused) != 800 || byTenant["one"] != 250 || byTenant["many"] != 250 {
		t.Fatalf("per tenant %v, refused %d", byTenant, len(refused))
	}
	for i := range 10 {
		if c := byLab["lab"+strconv.Itoa(i)]; c != 25 {
			t.Fatalf("the labs of a tenant share its part: %v", byLab)
		}
	}
	// without a tenant label the namespace is the tenant
	l := lineSpec(wire.RoleOwner, "")
	l.Tenant = ""
	a, _ := admitLines([]wire.LineSpec{l})
	if len(a) != 1 {
		t.Fatal("a line without a tenant")
	}
}
