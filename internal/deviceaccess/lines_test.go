package deviceaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func lineSpec(role, peer string) wire.LineSpec {
	return wire.LineSpec{Namespace: testNS, Name: "cable-1", UID: "cable-uid", Lab: testLab, LabUID: "lab-uid", Role: role, Peer: peer,
		Serial:  wire.LineEnd{Device: "pc1", Port: "serial0", Ports: portkeeper.Marshal(pc1Ports)},
		Console: wire.LineEnd{Device: "r1", Port: "console0", Ports: portkeeper.Marshal(r1Ports)}}
}

func lineService(t *testing.T, rt Runtime) *Service {
	t.Helper()
	lim := DefaultLimits()
	lim.WriteTimeout, lim.Cleanup = 2*time.Second, time.Second
	s := New(Config{Runtime: rt, Limits: lim, Log: logr.Discard(), CableKey: StaticCableKey(testCableKey)})
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
	a, b := lineService(t, rtA), lineService(t, rtB)
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

// A peer without the key is unreachable for the owner: its reply cannot be authentic.
func TestLineCrossNodeWrongKey(t *testing.T) {
	fastRetry(t)
	rtA, _, _ := lineRuntime(t, true, false)
	rtB, _, _ := lineRuntime(t, false, true)
	a := lineService(t, rtA)
	lim := DefaultLimits()
	b := New(Config{Runtime: rtB, Limits: lim, Log: logr.Discard(), CableKey: StaticCableKey([]byte(strings.Repeat("o", 48)))})
	srv := httptest.NewServer(b.CableHandler())
	t.Cleanup(srv.Close)
	b.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleSerial, "")}})
	a.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleOwner, strings.TrimPrefix(srv.URL, "http://"))}})
	waitFor(t, "PeerUnreachable", func() bool { return stateOf(a, "cable-1").Reason == wire.LinePeerUnreachable })
	if len(rtB.started()) != 0 {
		t.Fatal("the serial port was attached for a request with another key")
	}
}

// What a holder of the key that is not the owner of a real line can do on the acceptor: nothing. A request for another lab, another
// cable UID or another port, an unsigned one and a replayed one are refused, and nothing is run in the device.
func TestCableStreamRefusals(t *testing.T) {
	rt, _, _ := lineRuntime(t, false, true)
	s := lineService(t, rt)
	s.SyncLines(wire.LinesRequest{Lines: []wire.LineSpec{lineSpec(wire.RoleSerial, "")}})
	srv := httptest.NewServer(s.CableHandler())
	t.Cleanup(srv.Close)
	send := func(open wire.CableOpen) wire.CableReply {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+wire.CablePath, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
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
		return r
	}
	base := cableOpenOf(lineSpec(wire.RoleOwner, ""))
	for name, c := range map[string]struct {
		mod  func(*wire.CableOpen)
		want string
	}{
		"another lab":       {func(r *wire.CableOpen) { r.Lab = "lab2" }, wire.CodeNotFound},
		"another lab uid":   {func(r *wire.CableOpen) { r.LabUID = "x" }, wire.CodeNotFound},
		"another cable":     {func(r *wire.CableOpen) { r.UID = "x" }, wire.CodeNotFound},
		"another port":      {func(r *wire.CableOpen) { r.Serial.Port = "serial1" }, wire.CodeNotFound},
		"another namespace": {func(r *wire.CableOpen) { r.Namespace = "lab-group-b" }, wire.CodeNotFound},
	} {
		open := base
		c.mod(&open)
		signed, _ := signOpen(testCableKey, open, time.Now())
		if r := send(signed); r.OK || r.Error != c.want || checkReply(testCableKey, r, signed.Nonce) != nil {
			t.Errorf("%s: %+v", name, r)
		}
	}
	unsigned := base
	unsigned.V, unsigned.Time, unsigned.Nonce, unsigned.MAC = wire.RelayVersion, time.Now().UnixMilli(), strings.Repeat("a", 32), strings.Repeat("b", 64)
	if r := send(unsigned); r.OK || r.Error != wire.CodeUnauthorized {
		t.Errorf("unsigned: %+v", r)
	}
	if len(rt.started()) != 0 {
		t.Fatal("a refused request attached the serial port")
	}
	good, _ := signOpen(testCableKey, base, time.Now())
	if r := send(good); !r.OK {
		t.Fatalf("the real line: %+v", r)
	}
	if r := send(good); r.OK || r.Error != wire.CodeUnauthorized {
		t.Fatalf("a replayed request: %+v", r)
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
