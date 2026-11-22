package portkeeper

import (
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// A client that never reads must not block the program on the line: its queue is bounded and drops the oldest output, and the write
// deadline detaches it. The line stays usable for the next client.
func TestStalledClientNeverBlocksTheLine(t *testing.T) {
	dir := shortDir(t)
	end := newLineEnd()
	h := NewHub()
	h.SetLine(end)
	go func() { _ = h.Pump(end) }()
	sock := filepath.Join(dir, "console0.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go h.Serve(ln)

	stalled := dialAttached(t, sock) // attached, and never reads again
	defer func() { _ = stalled.Close() }()
	done := make(chan struct{})
	go func() {
		chunk := make([]byte, 32<<10)
		for i := 0; i < 512; i++ { // 16 MiB, far more than any socket buffer
			if _, err := end.pw.Write(chunk); err != nil {
				break
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the program on the line blocked behind a client that does not read")
	}
	// the write deadline (clientWriteTimeout) detaches it
	for deadline := time.Now().Add(clientWriteTimeout + 3*time.Second); h.Attached(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stalled client was never detached")
		}
	}
	c := dialAttached(t, sock)
	_ = c.Close()
}

type failingListener struct {
	calls  int
	closed chan struct{}
}

func (l *failingListener) Accept() (net.Conn, error) {
	l.calls++
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
		return nil, errors.New("accept: too many open files")
	}
}
func (l *failingListener) Close() error   { close(l.closed); return nil }
func (l *failingListener) Addr() net.Addr { return nil }

// Repeated accept errors (EMFILE) are waited out, not spun on.
func TestServeBacksOffOnAcceptErrors(t *testing.T) {
	l := &failingListener{closed: make(chan struct{})}
	done := make(chan struct{})
	go func() { NewHub().Serve(l); close(done) }()
	time.Sleep(300 * time.Millisecond)
	_ = l.Close()
	<-done
	if l.calls > 20 {
		t.Fatalf("%d accepts in 300 ms: the loop spins", l.calls)
	}
}

// The handshake is always the first byte a client gets, also while the line is busy writing.
func TestAttachAckComesFirst(t *testing.T) {
	dir := shortDir(t)
	h := NewHub()
	r, w := io.Pipe()
	go func() { _ = h.Pump(r) }()
	stop := make(chan struct{})
	go func() {
		b := []byte("xxxxxxxxxxxxxxxx")
		for {
			select {
			case <-stop:
				_ = w.Close()
				return
			default:
				_, _ = w.Write(b)
			}
		}
	}()
	defer close(stop)
	sock := filepath.Join(dir, "aux0.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go h.Serve(ln)
	for i := 0; i < 200; i++ {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 1)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(c, b); err != nil || b[0] != AttachOK {
			t.Fatalf("attach %d: first byte %q, %v", i, b, err)
		}
		_ = c.Close()
		eventually(t, "the line to be free", func() bool { return !h.Attached() })
	}
}
