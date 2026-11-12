package deviceaccess

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/names"
)

// fakeRuntime is a container runtime with a fixed set of containers. A started exec runs the behaviour the test gives it.
type fakeRuntime struct {
	mu         sync.Mutex
	containers []*fakeContainer
	execs      []*fakeExec
	behaviour  func(proc *specs.Process, io IO, ex *fakeExec)
	startErr   error
}

type fakeContainer struct {
	ContainerInfo
	running bool
	spec    *specs.Spec
}

func (r *fakeRuntime) add(c *fakeContainer) { r.containers = append(r.containers, c) }

func (r *fakeRuntime) ListByLabels(_ context.Context, match map[string]string) ([]ContainerInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ContainerInfo
outer:
	for _, c := range r.containers {
		for k, v := range match {
			if c.Labels[k] != v {
				continue outer
			}
		}
		out = append(out, c.ContainerInfo)
	}
	return out, nil
}

func (r *fakeRuntime) find(id string) *fakeContainer {
	for _, c := range r.containers {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (r *fakeRuntime) Running(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.find(id); c != nil {
		return c.running, nil
	}
	return false, nil
}

func (r *fakeRuntime) Spec(_ context.Context, id string) (*specs.Spec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.find(id); c != nil && c.spec != nil {
		return c.spec, nil
	}
	return nil, errors.New("no such container")
}

func (r *fakeRuntime) Start(_ context.Context, id string, proc *specs.Process, stdio IO) (Exec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return nil, r.startErr
	}
	ex := &fakeExec{container: id, proc: proc, done: make(chan ExitStatus, 1), killed: make(chan struct{})}
	r.execs = append(r.execs, ex)
	if r.behaviour != nil {
		go r.behaviour(proc, stdio, ex)
	}
	return ex, nil
}

func (r *fakeRuntime) allClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.execs {
		if e.closes.Load() == 0 {
			return false
		}
	}
	return true
}

func (r *fakeRuntime) started() []*fakeExec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*fakeExec(nil), r.execs...)
}

type fakeExec struct {
	container string
	proc      *specs.Process
	done      chan ExitStatus
	killed    chan struct{}
	closes    atomic.Int32
	resizes   atomic.Int32
	lastSize  atomic.Uint32
	once      sync.Once
}

func (e *fakeExec) exit(code uint32) {
	e.once.Do(func() { e.done <- ExitStatus{Code: code}; close(e.done) })
}
func (e *fakeExec) Wait() <-chan ExitStatus { return e.done }
func (e *fakeExec) Resize(_ context.Context, cols, rows uint16) error {
	e.resizes.Add(1)
	e.lastSize.Store(uint32(cols)<<16 | uint32(rows))
	return nil
}
func (e *fakeExec) Close(context.Context) error {
	if e.closes.Add(1) == 1 {
		close(e.killed)
		e.exit(137)
	}
	return nil
}

// echo is a shell that answers every byte of stdin with the same bytes, in capitals, until its stdin ends or it is killed.
func echo(_ *specs.Process, stdio IO, ex *fakeExec) {
	buf := make([]byte, 1024)
	for {
		n, err := stdio.Stdin.Read(buf)
		if n > 0 {
			up := make([]byte, n)
			for i, b := range buf[:n] {
				if b >= 'a' && b <= 'z' {
					b -= 32
				}
				up[i] = b
			}
			if _, werr := stdio.Stdout.Write(up); werr != nil {
				return
			}
		}
		if err != nil {
			ex.exit(0)
			return
		}
		select {
		case <-ex.killed:
			return
		default:
		}
	}
}

// pipeConn is the service side of an in-memory stream with the proxy.
type pipeConn struct {
	in  chan message // to the service
	out chan message // from the service
	cl  chan struct{}
	one sync.Once
}

type message struct {
	text bool
	data []byte
}

func newPipeConn() *pipeConn {
	return &pipeConn{in: make(chan message, 64), out: make(chan message, 64), cl: make(chan struct{})}
}

func (c *pipeConn) Read(ctx context.Context) (bool, []byte, error) {
	select {
	case m := <-c.in:
		return m.text, m.data, nil
	case <-ctx.Done():
		// Like the WebSocket library the service uses: a read whose context ends closes the connection.
		c.hangUp()
		return false, nil, ctx.Err()
	case <-c.cl:
		return false, nil, io.EOF
	}
}
func (c *pipeConn) write(ctx context.Context, m message) error {
	select {
	case c.out <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.cl:
		return io.ErrClosedPipe
	}
}
func (c *pipeConn) WriteText(ctx context.Context, b []byte) error {
	return c.write(ctx, message{true, append([]byte(nil), b...)})
}
func (c *pipeConn) WriteBinary(ctx context.Context, b []byte) error {
	return c.write(ctx, message{false, append([]byte(nil), b...)})
}
func (c *pipeConn) hangUp() { c.one.Do(func() { close(c.cl) }) }

// proxy side helpers
func (c *pipeConn) sendOpen(t *testing.T, r wire.OpenRequest) {
	t.Helper()
	b, _ := json.Marshal(r)
	c.in <- message{true, b}
}
func (c *pipeConn) send(b []byte) { c.in <- message{false, b} }
func (c *pipeConn) next(t *testing.T) message {
	t.Helper()
	select {
	case m := <-c.out:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no message from the service")
		return message{}
	}
}
func (c *pipeConn) reply(t *testing.T) wire.OpenReply {
	t.Helper()
	m := c.next(t)
	if !m.text {
		t.Fatalf("expected the open reply, got a frame %v", m.data)
	}
	var r wire.OpenReply
	if err := json.Unmarshal(m.data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// device pod fixtures

const (
	testNS   = "lab-group-a"
	testLab  = "lab1"
	testDev  = "kali"
	podUID   = "pod-uid-1"
	sandbox1 = "sb1"
	cont1    = "c1"
)

func sandboxLabels(uid string) map[string]string {
	return map[string]string{
		labelKind: kindSandbox, labelPodNamespace: testNS, labelPodUID: uid,
		names.LabelLab: testLab, names.LabelDevice: testDev,
		names.LabelLabUID: "lab-uid", names.LabelDeviceUID: "dev-uid",
		names.LabelTerminal: "true",
	}
}

func appLabels(uid, name string) map[string]string {
	return map[string]string{labelKind: kindContainer, labelPodNamespace: testNS, labelPodUID: uid, labelContainer: name}
}

func deviceSpec() *specs.Spec {
	return &specs.Spec{Process: &specs.Process{
		User: specs.User{UID: 1000, GID: 1000},
		Args: []string{"/entry"},
		Env:  []string{"PATH=/usr/bin", "FOO=bar"},
		Cwd:  "/home/x",
		Capabilities: &specs.LinuxCapabilities{
			Bounding:  []string{"CAP_CHOWN", "CAP_NET_BIND_SERVICE"},
			Effective: []string{"CAP_CHOWN"}, Permitted: []string{"CAP_CHOWN"}, Inheritable: []string{}, Ambient: []string{},
		},
		NoNewPrivileges: true,
	}}
}

// labRuntime has one valid, running, console-enabled device.
func labRuntime() *fakeRuntime {
	rt := &fakeRuntime{}
	rt.add(&fakeContainer{ContainerInfo: ContainerInfo{ID: sandbox1, Labels: sandboxLabels(podUID)}, running: true})
	rt.add(&fakeContainer{ContainerInfo: ContainerInfo{ID: cont1, Labels: appLabels(podUID, testDev)}, running: true, spec: deviceSpec()})
	rt.behaviour = echo
	return rt
}

func testService(rt Runtime) *Service {
	lim := DefaultLimits()
	lim.WriteTimeout = time.Second
	lim.Cleanup = time.Second
	return New(Config{Runtime: rt, Limits: lim, Log: logr.Discard()})
}

func openReq() wire.OpenRequest {
	return wire.OpenRequest{Kind: wire.KindConsole, Namespace: testNS, Lab: testLab, Device: testDev, Client: "c-1", Group: "g-1", Cols: 100, Rows: 30}
}
