package l7

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestActiveHTTPProgressAndActualResponseTime(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	h, srv, _, cookie := liveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("hello"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	meter := NewMeter("b", time.Now())
	h.WithAccounting(meter, func(string, string) (string, bool) { return "lab", true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/stream", nil)
	req.Host = "web-abc123.challenges.example.com"
	req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
	result := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			result <- resp
		} else {
			result <- nil
		}
	}()
	<-entered
	rows, _ := meter.Ledger(GroupNamespace("g1"))
	if len(rows) != 1 || rows[0].Attempts != 1 || rows[0].RespondedMs != 0 {
		close(release)
		t.Fatal("active attempt", rows)
	}
	responseAfter := time.Now().UnixMilli()
	close(release)
	resp := <-result
	if resp == nil {
		t.Fatal("missing response")
	}
	defer resp.Body.Close()
	b := make([]byte, 5)
	_, err := io.ReadFull(resp.Body, b)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ = meter.Ledger(GroupNamespace("g1"))
	if rows[0].BytesIn != 5 || rows[0].RespondedMs < responseAfter {
		t.Fatal("active response", rows)
	}
}

// Real TCP upgrade includes client frames buffered with the request headers.
func TestWebSocketCountsPayloadBothDirectionsAndBufferedInput(t *testing.T) {
	received := make(chan string, 1)
	h, srv, _, cookie := liveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		got := make([]byte, 9)
		_, err = io.ReadFull(rw, got)
		if err != nil {
			return
		}
		received <- string(got)
		_, _ = c.Write([]byte{0x81, 5, 'h', 'e', 'l', 'l', 'o', 0x89, 2, 'o', 'k'})
		_, _ = io.Copy(io.Discard, rw)
	}))
	meter := NewMeter("b", time.Now())
	h.WithAccounting(meter, func(string, string) (string, bool) { return "lab", true })
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	header := fmt.Sprintf("GET / HTTP/1.1\r\nHost: web-abc123.challenges.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nCookie: challenge=%s\r\n\r\n", cookie)
	frame := []byte{0x81, 0x83, 1, 2, 3, 4, 'a' ^ 1, 'b' ^ 2, 'c' ^ 3}
	_, _ = c.Write(append([]byte(header), frame...))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatal(resp, err)
	}
	got := make([]byte, 11)
	if _, err = io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string([]byte{0x81, 5, 'h', 'e', 'l', 'l', 'o', 0x89, 2, 'o', 'k'}) {
		t.Fatal("forwarding changed", got)
	}
	if got := <-received; got != string(frame) {
		t.Fatal("buffered input changed", got)
	}
	rows, _, partial := meter.Snapshot(GroupNamespace("g1"))
	if len(rows) != 1 || rows[0].BytesIn != 5 || rows[0].BytesOut != 3 || rows[0].RespondedMs == 0 || partial {
		t.Fatal(rows, partial)
	}
}
func TestFrameMeterFragmentControlCompressionAndMalformed(t *testing.T) {
	m := NewMeter("b", time.Now())
	r := m.Begin("ns", "p", "lab", time.Now())
	f := newFrameMeter(r.AddIn, r.Incomplete, false, true)
	// Compressed text, control ping between fragments, final continuation.
	wire := []byte{0x41, 2, 9, 8, 0x89, 1, 7, 0x80, 3, 6, 5, 4}
	for _, v := range wire {
		f.feed([]byte{v})
	}
	rows, _, partial := m.Snapshot("ns")
	if rows[0].BytesIn != 5 || partial {
		t.Fatal(rows, partial)
	}
	f.feed([]byte{0x82, 0x80}) // masked server frame: malformed, data stays forwarded by caller
	_, _, partial = m.Snapshot("ns")
	if !partial {
		t.Fatal("malformed frame not marked")
	}
	r.End()
}
func TestFrameMeterExtendedLengthAndTruncatedHeader(t *testing.T) {
	m := NewMeter("b", time.Now())
	r := m.Begin("ns", "p", "lab", time.Now())
	f := newFrameMeter(r.AddOut, r.Incomplete, true, false)
	wire := append([]byte{0x82, 0xfe, 0, 126, 1, 2, 3, 4}, []byte(strings.Repeat("x", 126))...)
	for i := 0; i < len(wire); i += 3 {
		end := i + 3
		if end > len(wire) {
			end = len(wire)
		}
		f.feed(wire[i:end])
	}
	rows, _, partial := m.Snapshot("ns")
	if rows[0].BytesOut != 126 || partial {
		t.Fatal(rows, partial)
	}
	f.feed([]byte{0x82})
	f.finish()
	_, _, partial = m.Snapshot("ns")
	if !partial {
		t.Fatal("truncated header not marked")
	}
	r.End()
}

type accountingRoundTripper func(*http.Request) (*http.Response, error)

func (f accountingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenBody struct{ sent bool }

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("broken body")
	}
	b.sent = true
	return copy(p, "abc"), nil
}
func (*brokenBody) Close() error { return nil }
func TestAbortKeepsProgressAndFinishesMeter(t *testing.T) {
	h, _, _, cookie := liveFixture(t, http.NotFoundHandler())
	m := NewMeter("b", time.Now())
	h.WithAccounting(m, func(string, string) (string, bool) { return "lab", true })
	h.transport = accountingRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &brokenBody{}, ContentLength: 10}, nil
	})
	req := httptest.NewRequest("GET", "http://web-abc123.challenges.example.com/", nil)
	req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
	req = req.WithContext(context.WithValue(req.Context(), http.ServerContextKey, &http.Server{}))
	var caught any
	func() { defer func() { caught = recover() }(); h.ServeHTTP(httptest.NewRecorder(), req) }()
	if caught != http.ErrAbortHandler {
		t.Fatalf("panic=%v", caught)
	}
	rows, _, partial := m.Snapshot(GroupNamespace("g1"))
	if len(rows) != 1 || rows[0].Attempts != 1 || rows[0].BytesIn != 3 || !partial || len(h.live.snapshot()) != 0 {
		t.Fatal(rows, partial)
	}
}
func TestFrameMeter64BitLengthAndUnknownExtension(t *testing.T) {
	m := NewMeter("b", time.Now())
	r := m.Begin("ns", "p", "lab", time.Now())
	f := newFrameMeter(r.AddIn, r.Incomplete, false, false)
	header := []byte{0x82, 127, 0, 0, 0, 0, 0, 1, 0, 0}
	f.feed(header)
	f.feed(make([]byte, 65536))
	f.finish()
	rows, _, partial := m.Snapshot("ns")
	if rows[0].BytesIn != 65536 || partial {
		t.Fatal(rows, partial)
	}
	if _, known := websocketCompression("permessage-deflate, x-private"); known {
		t.Fatal("unsupported extension accepted")
	}
	r.End()
}
