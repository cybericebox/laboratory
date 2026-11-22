package portkeeper

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// shortDir is a directory for unix sockets: the path of a socket is limited to about 100 bytes, and t.TempDir can be longer on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// lineEnd is the keeper's side of a line in a test: what the hub reads from (the program's output) and writes to (its input).
type lineEnd struct {
	out *io.PipeReader // program output, read by the hub
	in  *bytes.Buffer  // program input, written by the hub
	mu  sync.Mutex
	pw  *io.PipeWriter
}

func newLineEnd() *lineEnd {
	r, w := io.Pipe()
	return &lineEnd{out: r, pw: w, in: &bytes.Buffer{}}
}
func (l *lineEnd) Read(p []byte) (int, error) { return l.out.Read(p) }
func (l *lineEnd) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.in.Write(p)
}
func (l *lineEnd) input() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.in.String()
}

func dialAttached(t *testing.T, sock string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil || b[0] != AttachOK {
		t.Fatalf("handshake: %v %v", b, err)
	}
	return c
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// One client per port: bytes go both ways while it is attached; a second attach is refused (closed without the handshake); output is
// dropped while nobody is attached, so the program never blocks; a new client gets only what comes after it attached.
func TestHubOneClientDropsWhileUnattached(t *testing.T) {
	dir := shortDir(t)
	end := newLineEnd()
	h := NewHub()
	h.SetLine(end)
	go h.Pump(end)
	sock := filepath.Join(dir, "console0.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go h.Serve(ln)

	// unattached: the program writes and is not blocked
	done := make(chan struct{})
	go func() { _, _ = end.pw.Write([]byte("lost output\n")); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the program blocked while no client was attached")
	}
	time.Sleep(100 * time.Millisecond) // the pump hands the bytes on right after it read them: let it drop them before anyone attaches

	c := dialAttached(t, sock)
	// a second attach is refused
	c2, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := c2.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("a second client must be refused: %d %v", n, err)
	}
	_ = c2.Close()

	if _, err := c.Write([]byte("show run\n")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "input on the line", func() bool { return end.input() == "show run\n" })
	go func() { _, _ = end.pw.Write([]byte("hostname r1\n")) }()
	got := make([]byte, len("hostname r1\n"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "hostname r1\n" {
		t.Fatalf("output: %q %v", got, err)
	}
	_ = c.Close()

	// after the client left, a new one can attach
	eventually(t, "the line to be free", func() bool { return !h.Attached() })
	c3 := dialAttached(t, sock)
	_ = c3.Close()
}

// A line without an end (a tcp console that is not connected) drops what the client types.
func TestHubDropsInputWithoutALine(t *testing.T) {
	h := NewHub()
	if n, err := h.WriteLine([]byte("x")); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
}
