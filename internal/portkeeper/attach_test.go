package portkeeper

import (
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// port-attach connects the stdio of the device access service's exec to a port: the bytes go both ways, an unknown port name is refused
// before anything is dialled, a busy line ends with ExitBusy, and a missing keeper is started once and the dial retried.
func TestAttach(t *testing.T) {
	dir := shortDir(t)
	h := NewHub()
	end := newLineEnd()
	h.SetLine(end)
	go func() { _ = h.Pump(end) }()

	if code := Attach(dir, "../etc/passwd", strings.NewReader(""), io.Discard, nil); code != ExitUsage {
		t.Fatalf("a bad port name: %d", code)
	}

	// no keeper yet: the attach starts it (here: the test starts serving), then connects
	started := 0
	start := func() error {
		started++
		ln, err := Listen(SocketPath(dir, "console0"))
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = ln.Close() })
		go h.Serve(ln)
		return nil
	}
	in, inW := io.Pipe()
	var out safeBuffer
	done := make(chan int, 1)
	go func() { done <- Attach(dir, "console0", in, &out, start) }()
	if _, err := inW.Write([]byte("enable\n")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the bytes on the line", func() bool { return end.input() == "enable\n" })
	go func() { _, _ = end.pw.Write([]byte("r1#")) }()
	eventually(t, "the line output", func() bool { return out.String() == "r1#" })
	if started != 1 {
		t.Fatalf("the keeper was started %d times", started)
	}

	// while it is attached, a second attach is busy
	if code := Attach(dir, "console0", strings.NewReader(""), io.Discard, nil); code != ExitBusy {
		t.Fatalf("a second attach: %d", code)
	}
	_ = inW.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("attach ended with %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the attach did not end with its input")
	}

	// nobody serves and nothing can start a keeper
	if code := Attach(shortDir(t), "console0", strings.NewReader(""), io.Discard, func() error { return net.ErrClosed }); code != ExitNoKeeper {
		t.Fatalf("no keeper: %d", code)
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *safeBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
