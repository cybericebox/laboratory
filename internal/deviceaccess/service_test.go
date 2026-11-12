package deviceaccess

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// run starts Serve on a fresh connection and returns it with a channel closed when Serve returns.
func run(svc *Service, ctx context.Context) (*pipeConn, <-chan struct{}) {
	c := newPipeConn()
	done := make(chan struct{})
	go func() { svc.Serve(ctx, c); close(done) }()
	return c, done
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the service did not finish the stream")
	}
}

func readData(t *testing.T, c *pipeConn, want string) {
	t.Helper()
	var got []byte
	for len(got) < len(want) {
		m := c.next(t)
		typ, payload, err := wire.Parse(m.data)
		if m.text || err != nil || typ != wire.TypeData {
			t.Fatalf("expected data, got %+v (%v)", m, err)
		}
		got = append(got, payload...)
	}
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func expectClose(t *testing.T, c *pipeConn, reason string) {
	t.Helper()
	for {
		m := c.next(t)
		if m.text {
			continue
		}
		typ, payload, err := wire.Parse(m.data)
		if err != nil {
			t.Fatal(err)
		}
		if typ == wire.TypeClose {
			if string(payload) != reason {
				t.Fatalf("closed with %q, want %q", payload, reason)
			}
			return
		}
	}
}

func TestConsoleRoundTripResizeAndLeave(t *testing.T) {
	rt := labRuntime()
	svc := testService(rt)
	c, done := run(svc, context.Background())
	c.sendOpen(t, openReq())
	if r := c.reply(t); !r.OK {
		t.Fatalf("refused: %+v", r)
	}
	c.send(wire.Data([]byte("whoami\n")))
	readData(t, c, "WHOAMI\n")
	c.send(wire.Resize(120, 40))
	c.send(wire.Close(wire.ReasonClient))
	expectClose(t, c, wire.ReasonClient)
	waitDone(t, done)

	ex := rt.started()
	if len(ex) != 1 {
		t.Fatalf("%d execs", len(ex))
	}
	if ex[0].closes.Load() != 1 {
		t.Fatal("the exec was not removed")
	}
	if got := ex[0].lastSize.Load(); got != 120<<16|40 {
		t.Fatalf("size %x", got)
	}
	if !ex[0].proc.Terminal || ex[0].proc.ConsoleSize.Width != 100 {
		t.Fatalf("process: %+v", ex[0].proc)
	}
	if v := testutil.ToFloat64(svc.m.bytes.WithLabelValues("console", "in")); v != 7 {
		t.Fatalf("bytes in %v", v)
	}
	if v := testutil.ToFloat64(svc.m.bytes.WithLabelValues("console", "out")); v != 7 {
		t.Fatalf("bytes out %v", v)
	}
	if v := testutil.ToFloat64(svc.m.sessions.WithLabelValues("console", wire.ReasonClient)); v != 1 {
		t.Fatalf("sessions %v", v)
	}
	if v := testutil.ToFloat64(svc.m.active.WithLabelValues("console")); v != 0 {
		t.Fatalf("still active: %v", v)
	}
}

func TestConsoleCleansUpWhenTheProxyVanishes(t *testing.T) {
	rt := labRuntime()
	svc := testService(rt)
	c, done := run(svc, context.Background())
	c.sendOpen(t, openReq())
	c.reply(t)
	c.hangUp()
	waitDone(t, done)
	if !rt.allClosed() {
		t.Fatal("exec left behind")
	}
}

func TestConsoleCleansUpOnABadFrame(t *testing.T) {
	rt := labRuntime()
	svc := testService(rt)
	c, done := run(svc, context.Background())
	c.sendOpen(t, openReq())
	c.reply(t)
	c.send([]byte{99, 1, 2})
	expectClose(t, c, wire.ReasonError)
	waitDone(t, done)
	if !rt.allClosed() {
		t.Fatal("exec left behind")
	}
}

func TestConsoleEndsWhenTheShellExits(t *testing.T) {
	rt := labRuntime()
	rt.behaviour = func(_ *specs.Process, _ IO, ex *fakeExec) { ex.exit(0) }
	svc := testService(rt)
	c, done := run(svc, context.Background())
	c.sendOpen(t, openReq())
	c.reply(t)
	expectClose(t, c, wire.ReasonExit)
	waitDone(t, done)
	if !rt.allClosed() {
		t.Fatal("exec left behind")
	}
}

func TestConsoleIdleAndLifetime(t *testing.T) {
	for name, set := range map[string]func(*Limits){
		wire.ReasonIdle:     func(l *Limits) { l.Idle = 60 * time.Millisecond },
		wire.ReasonLifetime: func(l *Limits) { l.Lifetime = 80 * time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			rt := labRuntime()
			svc := testService(rt)
			set(&svc.lim)
			c, done := run(svc, context.Background())
			c.sendOpen(t, openReq())
			c.reply(t)
			expectClose(t, c, name)
			waitDone(t, done)
			if !rt.allClosed() {
				t.Fatal("exec left behind")
			}
		})
	}
}

func TestConsoleActivityKeepsItFromIdling(t *testing.T) {
	rt := labRuntime()
	svc := testService(rt)
	svc.lim.Idle = 150 * time.Millisecond
	c, done := run(svc, context.Background())
	c.sendOpen(t, openReq())
	c.reply(t)
	for range 5 {
		time.Sleep(60 * time.Millisecond)
		c.send(wire.Data([]byte("a")))
		readData(t, c, "A")
	}
	c.send(wire.Close(wire.ReasonClient))
	expectClose(t, c, wire.ReasonClient)
	waitDone(t, done)
}

func TestConsoleStalledOutput(t *testing.T) {
	// The proxy does not read: the service cuts the stream after the write deadline instead of holding the exec for ever.
	rt := labRuntime()
	svc := testService(rt)
	svc.lim.WriteTimeout = 100 * time.Millisecond
	c := newPipeConn()
	c.out = make(chan message) // unbuffered and never read after the reply
	done := make(chan struct{})
	go func() { svc.Serve(context.Background(), c); close(done) }()
	c.sendOpen(t, openReq())
	if r := c.reply(t); !r.OK {
		t.Fatal("refused")
	}
	c.send(wire.Data([]byte("x")))
	waitDone(t, done)
	if !rt.allClosed() {
		t.Fatal("exec left behind")
	}
	if v := testutil.ToFloat64(svc.m.sessions.WithLabelValues("console", wire.ReasonStalled)); v != 1 {
		t.Fatalf("reason: %v", v)
	}
}

func TestShutdownRemovesRunningExecs(t *testing.T) {
	rt := labRuntime()
	svc := testService(rt)
	c, done := run(svc, context.Background())
	c.sendOpen(t, openReq())
	c.reply(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	waitDone(t, done)
	if !rt.allClosed() {
		t.Fatal("exec left behind")
	}
	// after shutdown nothing new starts
	c2, done2 := run(svc, context.Background())
	waitDone(t, done2)
	_ = c2
	if len(rt.started()) != 1 {
		t.Fatal("a stream started after the shutdown")
	}
}

func TestServeRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*wire.OpenRequest)
		rt     func(*fakeRuntime)
		code   string
	}{
		"wrong namespace":     {mutate: func(r *wire.OpenRequest) { r.Namespace = "lab-group-b" }, code: "not-found"},
		"unknown device":      {mutate: func(r *wire.OpenRequest) { r.Device = "nope" }, code: "not-found"},
		"console not enabled": {rt: func(rt *fakeRuntime) { delete(rt.containers[0].Labels, "laboratory.cybericebox.com/terminal") }, code: "disabled"},
		"VPN pod":             {rt: func(rt *fakeRuntime) { rt.containers[0].Labels["laboratory.cybericebox.com/component"] = "vpn" }, code: "not-found"},
		"system pod":          {mutate: func(r *wire.OpenRequest) { r.Namespace, r.Lab, r.Device = "kube-system", "coredns", "coredns" }, code: "not-found"},
		"bad user":            {mutate: func(r *wire.OpenRequest) { r.User = "root;id" }, code: "bad-request"},
		"bad size":            {mutate: func(r *wire.OpenRequest) { r.Cols = 9000 }, code: "bad-request"},
		"bad kind":            {mutate: func(r *wire.OpenRequest) { r.Kind = "shell" }, code: "bad-request"},
		"runtime fails":       {rt: func(rt *fakeRuntime) { rt.startErr = context.DeadlineExceeded }, code: "unavailable"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rt := labRuntime()
			if c.rt != nil {
				c.rt(rt)
			}
			svc := testService(rt)
			conn, done := run(svc, context.Background())
			req := openReq()
			if c.mutate != nil {
				c.mutate(&req)
			}
			conn.sendOpen(t, req)
			r := conn.reply(t)
			if r.OK || r.Error != c.code {
				t.Fatalf("reply %+v, want %q", r, c.code)
			}
			waitDone(t, done)
			if len(rt.started()) != 0 && c.code != "unavailable" {
				t.Fatal("an exec was started for a refused request")
			}
		})
	}
}

func TestServeRefusesBadOpenMessages(t *testing.T) {
	for name, msg := range map[string]message{
		"binary first":  {false, []byte{1}},
		"not json":      {true, []byte("hello")},
		"unknown field": {true, []byte(`{"kind":"console","namespace":"a","lab":"b","device":"c","containerId":"c1"}`)},
		"no device":     {true, []byte(`{"kind":"console","namespace":"a","lab":"b"}`)},
		"huge":          {true, append([]byte(`{"kind":"console","namespace":"`), bytes.Repeat([]byte("a"), wire.MaxOpen)...)},
	} {
		t.Run(name, func(t *testing.T) {
			rt := labRuntime()
			svc := testService(rt)
			c, done := run(svc, context.Background())
			c.in <- msg
			if r := c.reply(t); r.OK || r.Error != "bad-request" {
				t.Fatalf("reply %+v", r)
			}
			waitDone(t, done)
			if len(rt.started()) != 0 {
				t.Fatal("started")
			}
		})
	}
}

func TestConsoleLimits(t *testing.T) {
	rt := labRuntime()
	svc := testService(rt)
	svc.lim.PerClient, svc.lim.PerLab, svc.lim.PerNode = 2, 3, 4
	open := func(r wire.OpenRequest) (*pipeConn, <-chan struct{}, wire.OpenReply) {
		c, done := run(svc, context.Background())
		c.sendOpen(t, r)
		return c, done, c.reply(t)
	}
	a1, _, r := open(openReq())
	if !r.OK {
		t.Fatalf("a1 %+v", r)
	}
	a2, _, r := open(openReq())
	if !r.OK {
		t.Fatalf("a2 %+v", r)
	}
	// a third console of the same client: over the per client limit
	if _, _, r := open(openReq()); r.OK || r.Error != "limit" {
		t.Fatalf("per client: %+v", r)
	}
	other := openReq()
	other.Client = "c-2"
	b1, _, r := open(other)
	if !r.OK {
		t.Fatalf("b1 %+v", r)
	}
	// a fourth console in the same lab: over the per lab limit (3)
	other.Client = "c-3"
	if _, _, r := open(other); r.OK || r.Error != "limit" {
		t.Fatalf("per lab: %+v", r)
	}
	// a console leaves, its slot is free again
	a1.send(wire.Close(wire.ReasonClient))
	expectClose(t, a1, wire.ReasonClient)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, r := open(other); r.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was not freed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _ = a2, b1
}

func TestPerNodeLimit(t *testing.T) {
	var l limiter
	l.perClient, l.perLab = map[string]int{}, map[string]int{}
	lim := Limits{PerNode: 2}
	if !l.acquireConsole(lim, "a", "x") || !l.acquireConsole(lim, "b", "y") || l.acquireConsole(lim, "c", "z") {
		t.Fatal("per node limit")
	}
	l.releaseConsole("a", "x")
	if !l.acquireConsole(lim, "c", "z") {
		t.Fatal("slot not freed")
	}
}

func TestTapSeesBothDirectionsAndNothingElseLeaks(t *testing.T) {
	rt := labRuntime()
	svc := testService(rt)
	var mu sync.Mutex
	var in, out []byte
	svc.tap = func(info StreamInfo, dir Direction, p []byte) {
		mu.Lock()
		defer mu.Unlock()
		if info.Lab != testLab || info.Device != testDev {
			t.Errorf("info %+v", info)
		}
		if dir == ToDevice {
			in = append(in, p...)
		} else {
			out = append(out, p...)
		}
	}
	c, done := run(svc, context.Background())
	c.sendOpen(t, openReq())
	c.reply(t)
	c.send(wire.Data([]byte("ab")))
	readData(t, c, "AB")
	c.send(wire.Close(wire.ReasonClient))
	waitDone(t, done)
	mu.Lock()
	defer mu.Unlock()
	if string(in) != "ab" || string(out) != "AB" {
		t.Fatalf("tap: %q %q", in, out)
	}
}

// ---- file transfer ----

// probeFile is the fake probe: for put it stores stdin, for get it answers the header and the content.
func probeFile(store map[string][]byte, mu *sync.Mutex, getExit uint32) func(*specs.Process, IO, *fakeExec) {
	return func(p *specs.Process, stdio IO, ex *fakeExec) {
		if len(p.Args) < 3 {
			ex.exit(2)
			return
		}
		switch p.Args[1] {
		case "put":
			var buf bytes.Buffer
			b := make([]byte, 100)
			for {
				n, err := stdio.Stdin.Read(b)
				buf.Write(b[:n])
				if err != nil {
					break
				}
			}
			mu.Lock()
			store[p.Args[2]] = buf.Bytes()
			mu.Unlock()
			ex.exit(0)
		case "get":
			mu.Lock()
			data, ok := store[p.Args[2]]
			mu.Unlock()
			if !ok {
				ex.exit(10)
				return
			}
			var head [8]byte
			binary.BigEndian.PutUint64(head[:], uint64(len(data)))
			_, _ = stdio.Stdout.Write(head[:])
			_, _ = stdio.Stdout.Write(data)
			ex.exit(getExit)
		}
	}
}

func fileReq(kind, path string, size int64) wire.OpenRequest {
	r := openReq()
	r.Kind, r.Path, r.Size = kind, path, size
	return r
}

func TestUploadThenDownload(t *testing.T) {
	rt := labRuntime()
	store := map[string][]byte{}
	var mu sync.Mutex
	rt.behaviour = probeFile(store, &mu, 0)
	svc := testService(rt)

	content := strings.Repeat("0123456789", 5000) // larger than one frame
	c, done := run(svc, context.Background())
	c.sendOpen(t, fileReq(wire.KindPut, "/tmp/a.txt", int64(len(content))))
	if r := c.reply(t); !r.OK {
		t.Fatalf("%+v", r)
	}
	for b := []byte(content); len(b) > 0; {
		n := min(len(b), wire.MaxData)
		c.send(wire.Data(b[:n]))
		b = b[n:]
	}
	c.send(wire.Close(wire.ReasonExit))
	if r := c.reply(t); !r.OK {
		t.Fatalf("final reply %+v", r)
	}
	waitDone(t, done)
	if string(store["/tmp/a.txt"]) != content {
		t.Fatal("content differs")
	}
	// the user is the same as the console's: no user in the request keeps the container's
	if got := rt.started()[0].proc.User.UID; got != 1000 {
		t.Fatalf("uid %d", got)
	}

	c, done = run(svc, context.Background())
	c.sendOpen(t, fileReq(wire.KindGet, "/tmp/a.txt", 0))
	r := c.reply(t)
	if !r.OK || r.Size != int64(len(content)) {
		t.Fatalf("%+v", r)
	}
	readData(t, c, content)
	expectClose(t, c, wire.ReasonExit)
	waitDone(t, done)
	if !rt.allClosed() {
		t.Fatal("exec left behind")
	}
}

func TestUploadRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		req  wire.OpenRequest
		code string
	}{
		"relative path": {fileReq(wire.KindPut, "tmp/a", 1), "bad-request"},
		"empty path":    {fileReq(wire.KindPut, "", 1), "bad-request"},
		"dot dot":       {fileReq(wire.KindPut, "/tmp/../etc/x", 1), "bad-request"},
		"too big":       {fileReq(wire.KindPut, "/tmp/a", 17<<20), "too-big"},
		"negative":      {fileReq(wire.KindPut, "/tmp/a", -1), "bad-request"},
		"no console":    {func() wire.OpenRequest { r := fileReq(wire.KindPut, "/tmp/a", 1); r.Device = "nope"; return r }(), "not-found"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := labRuntime()
			svc := testService(rt)
			conn, done := run(svc, context.Background())
			conn.sendOpen(t, c.req)
			if r := conn.reply(t); r.OK || r.Error != c.code {
				t.Fatalf("%+v want %s", r, c.code)
			}
			waitDone(t, done)
			if len(rt.started()) != 0 {
				t.Fatal("started")
			}
		})
	}
}

func TestUploadWrongLength(t *testing.T) {
	for name, send := range map[string]int{"short": 3, "long": 12} {
		t.Run(name, func(t *testing.T) {
			rt := labRuntime()
			store := map[string][]byte{}
			var mu sync.Mutex
			rt.behaviour = probeFile(store, &mu, 0)
			svc := testService(rt)
			c, done := run(svc, context.Background())
			c.sendOpen(t, fileReq(wire.KindPut, "/tmp/a", 8))
			c.reply(t)
			c.send(wire.Data(bytes.Repeat([]byte("x"), send)))
			c.send(wire.Close(wire.ReasonExit))
			if r := c.reply(t); r.OK {
				t.Fatalf("accepted a %s upload", name)
			}
			waitDone(t, done)
			if !rt.allClosed() {
				t.Fatal("exec left behind")
			}
		})
	}
}

func TestDownloadRefusals(t *testing.T) {
	rt := labRuntime()
	store := map[string][]byte{"/big": bytes.Repeat([]byte("x"), 100)}
	var mu sync.Mutex
	rt.behaviour = probeFile(store, &mu, 0)
	svc := testService(rt)
	svc.lim.MaxFile = 50

	c, done := run(svc, context.Background())
	c.sendOpen(t, fileReq(wire.KindGet, "/missing", 0))
	if r := c.reply(t); r.OK || r.Error != "not-found" {
		t.Fatalf("%+v", r)
	}
	waitDone(t, done)

	c, done = run(svc, context.Background())
	c.sendOpen(t, fileReq(wire.KindGet, "/big", 0))
	if r := c.reply(t); r.OK || r.Error != "too-big" {
		t.Fatalf("%+v", r)
	}
	waitDone(t, done)
	if !rt.allClosed() {
		t.Fatal("exec left behind")
	}
}

func TestDownloadThatFailsMidwayIsMarked(t *testing.T) {
	rt := labRuntime()
	store := map[string][]byte{"/f": []byte("abc")}
	var mu sync.Mutex
	rt.behaviour = probeFile(store, &mu, 14)
	svc := testService(rt)
	c, done := run(svc, context.Background())
	c.sendOpen(t, fileReq(wire.KindGet, "/f", 0))
	if r := c.reply(t); !r.OK {
		t.Fatalf("%+v", r)
	}
	readData(t, c, "abc")
	expectClose(t, c, wire.ReasonError)
	waitDone(t, done)
}

func TestTransferLimit(t *testing.T) {
	var l limiter
	l.transfersPerClient = map[string]int{}
	lim := Limits{Transfers: 3, TransfersPerClient: 2}
	if !l.acquireTransfer(lim, "a") || !l.acquireTransfer(lim, "a") || l.acquireTransfer(lim, "a") {
		t.Fatal("per client")
	}
	if !l.acquireTransfer(lim, "b") || l.acquireTransfer(lim, "c") {
		t.Fatal("per node")
	}
	l.releaseTransfer("a")
	if !l.acquireTransfer(lim, "c") {
		t.Fatal("not freed")
	}
}
