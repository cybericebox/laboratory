package devicestate

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

func TestDebounceFiresOnceAfterQuiet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan struct{}, 1)
	var fired atomic.Int32
	go Debounce(ctx, in, 60*time.Millisecond, time.Hour, func() { fired.Add(1) })
	for i := 0; i < 5; i++ {
		in <- struct{}{}
		time.Sleep(15 * time.Millisecond)
	}
	if fired.Load() != 0 {
		t.Fatal("must not fire while signals keep coming")
	}
	time.Sleep(200 * time.Millisecond)
	if fired.Load() != 1 {
		t.Fatalf("want one trigger after the quiet period, got %d", fired.Load())
	}
}

func TestDebounceMaxWaitFiresUnderContinuousChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan struct{}, 1)
	var fired atomic.Int32
	go Debounce(ctx, in, 100*time.Millisecond, 150*time.Millisecond, func() { fired.Add(1) })
	end := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(end) {
		select {
		case in <- struct{}{}:
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Fatal("a constantly written layer must still be snapshotted")
	}
}

func recv(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// Past the cap on watched directories the layer is still noticed, by the periodic scan.
func TestWatchFallsBackToPollingPastTheDirectoryCap(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("d%02d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := Watch(ctx, root, snapshot.NewPolicy(0, nil, 0, 0), 100*time.Millisecond, 2) // only 2 of the 10 are watched
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d09", "late"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !recv(ch, 3*time.Second) {
		t.Fatal("a change in an unwatched directory must be found by the periodic scan")
	}
}

func TestWatchBudget(t *testing.T) {
	b := &watchBudget{max: 2}
	if !b.take() || !b.take() || b.take() {
		t.Fatal("the budget allows exactly max watches")
	}
}

func TestWatchSignalsChangesButNotExcludedOrExisting(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	pol := snapshot.NewPolicy(0, nil, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := Watch(ctx, root, pol, 100*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if recv(ch, 300*time.Millisecond) {
		t.Fatal("what is in the layer at the start is not a change")
	}
	if err := os.WriteFile(filepath.Join(root, "tmp", "scratch"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if recv(ch, 400*time.Millisecond) {
		t.Fatal("changes under an excluded path must not signal")
	}
	if err := os.MkdirAll(filepath.Join(root, "srv", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !recv(ch, 2*time.Second) {
		t.Fatal("a new directory must signal")
	}
	time.Sleep(200 * time.Millisecond)
	for len(ch) > 0 {
		<-ch
	}
	if err := os.WriteFile(filepath.Join(root, "srv", "data", "db"), []byte("rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !recv(ch, 2*time.Second) {
		t.Fatal("a file written in a directory created after the start must signal")
	}
}

func TestFingerprintDetectsChangeAndIgnoresExcluded(t *testing.T) {
	root := t.TempDir()
	pol := snapshot.NewPolicy(0, nil, 0, 0)
	a := Fingerprint(root, pol)
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tmp", "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Fingerprint(root, pol) != a {
		t.Fatal("excluded paths must not change the fingerprint")
	}
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := Fingerprint(root, pol)
	if b == a {
		t.Fatal("a new file must change the fingerprint")
	}
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("longer content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Fingerprint(root, pol) == b {
		t.Fatal("a modified file must change the fingerprint")
	}
}

func TestForwardRelaysToTarget(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := probe.Addr().String()
	probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Forward(ctx, addr, up.Addr().String(), logr.Discard()) }()
	var c net.Conn
	for i := 0; i < 50; i++ {
		if c, err = net.Dial("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("got %q %v", buf, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Forward must return when ctx ends")
	}
}
