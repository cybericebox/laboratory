package l7

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

func testHandler() *Handler { return NewHandler(nil, nil, "example.test", "c", nil) }

func TestNoPacerWithoutLimits(t *testing.T) {
	h := testHandler()
	if h.pacerFor(context.Background(), "g", "c") != nil {
		t.Fatal("no limits, no pacer: the common case costs nothing")
	}
	var p *pacer
	if err := p.wait(1 << 20); err != nil {
		t.Fatal("a nil pacer never waits")
	}
}

func TestResponseIsPacedToTheParticipantsCeiling(t *testing.T) {
	h := testHandler()
	h.Throttle().SetLimits(0, 800) // 100 kB/s, burst 16 kB
	p := h.pacerFor(context.Background(), "g", "c")
	if p == nil {
		t.Fatal("a pacer once the limits are set")
	}
	rec := httptest.NewRecorder()
	w := &countingWriter{ResponseWriter: rec, request: newRequest(), pace: p}
	start := time.Now()
	for i := 0; i < 10; i++ {
		if _, err := w.Write(bytes.Repeat([]byte{'x'}, 4000)); err != nil {
			t.Fatal(err)
		}
	}
	// 40 kB, the first 16 kB are the burst: about 0.24 s at 100 kB/s
	if d := time.Since(start); d < 180*time.Millisecond || d > time.Second {
		t.Fatalf("40 kB took %v", d)
	}
	if rec.Body.Len() != 40000 {
		t.Fatalf("everything is delivered, only later: %d", rec.Body.Len())
	}
}

func TestRequestBodyIsPacedToo(t *testing.T) {
	h := testHandler()
	h.Throttle().SetLimits(0, 800)
	p := h.pacerFor(context.Background(), "g", "c")
	b := &countingBody{ReadCloser: io.NopCloser(bytes.NewReader(make([]byte, 40000))), request: newRequest(), pace: p}
	start := time.Now()
	n, err := io.Copy(io.Discard, b)
	if err != nil || n != 40000 {
		t.Fatalf("%d %v", n, err)
	}
	if d := time.Since(start); d < 180*time.Millisecond || d > time.Second {
		t.Fatalf("40 kB upload took %v", d)
	}
}

func TestPacingStopsWhenTheRequestIsCancelled(t *testing.T) {
	h := testHandler()
	h.Throttle().SetLimits(0, 80) // 10 kB/s: a long wait
	ctx, cancel := context.WithCancel(context.Background())
	p := h.pacerFor(ctx, "g", "c")
	p.wait(16384) // the burst
	done := make(chan error, 1)
	go func() { done <- p.wait(100000) }() // ten seconds
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled request must stop waiting with an error")
		}
	case <-time.After(time.Second):
		t.Fatal("the wait did not end with the request")
	}
}

func TestParticipantsShareOneBucketAcrossRequests(t *testing.T) {
	h := testHandler()
	h.Throttle().SetLimits(0, 800)
	a := h.pacerFor(context.Background(), "g", "c")
	b := h.pacerFor(context.Background(), "g", "c")
	other := h.pacerFor(context.Background(), "g", "d")
	a.wait(16384) // the same participant's burst is spent by the first request
	if d := h.Throttle().Delay(b.key, 10000); d < 50*time.Millisecond {
		t.Fatalf("the second request of the same participant must wait: %v", d)
	}
	if d := h.Throttle().Delay(other.key, 1000); d != 0 {
		t.Fatalf("another participant has its own bucket: %v", d)
	}
}

func TestPacedConnDeliversWhatWasAlreadyReadFirst(t *testing.T) {
	h := testHandler()
	h.Throttle().SetLimits(0, 80000)
	p := h.pacerFor(context.Background(), "g", "c")
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	buffered := bytes.NewReader([]byte("early"))
	c := &pacedConn{Conn: a, r: io.MultiReader(buffered, a), pace: p}
	go b.Write([]byte("-late"))
	got := make([]byte, 0, 16)
	buf := make([]byte, 16)
	for len(got) < 10 {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(got) != "early-late" {
		t.Fatalf("%q", got)
	}
}

func newRequest() *RequestMeter {
	return NewMeter("boot", time.Now()).Begin("ns", "c", "lab", time.Now())
}
