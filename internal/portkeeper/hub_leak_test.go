package portkeeper

import (
	"io"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A detached client leaves nothing behind: over many attach/detach cycles of a busy line the number of goroutines does not grow.
func TestDetachedClientsLeaveNoGoroutine(t *testing.T) {
	dir := shortDir(t)
	h := NewHub()
	r, w := io.Pipe()
	go func() { _ = h.Pump(r) }()
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				_ = w.Close()
				return
			default:
				_, _ = w.Write([]byte("output"))
				time.Sleep(time.Millisecond)
			}
		}
	}()
	defer close(stop)
	sock := filepath.Join(dir, "console0.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go h.Serve(ln)

	cycle := func() {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadFull(c, make([]byte, 1))
		_ = c.Close()
		eventually(t, "the line to be free", func() bool { return !h.Attached() })
	}
	for i := 0; i < 20; i++ {
		cycle()
	}
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 300; i++ {
		cycle()
	}
	var after int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if after = runtime.NumGoroutine(); after <= before+5 {
			return
		}
	}
	t.Fatalf("goroutines grew from %d to %d over 300 attach/detach cycles", before, after)
}
