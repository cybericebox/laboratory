package l7

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestLateHijackAfterCloseIsImmediatelyClosed(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	e := &liveEntry{cancel: func() {}}
	e.close()
	e.setConn(a)
	_ = b.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("late connection remains open")
	}
}
func TestShutdownClosesAndWaitsForCounters(t *testing.T) {
	h := NewHandler(nil, nil, "", "", nil)
	m := NewMeter("b", time.Now())
	r := m.Begin("ns", "p", "lab", time.Now())
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	e := &liveEntry{group: "g", client: "p", cancel: cancel}
	e.setConn(a)
	h.live.tryAdd(e, LiveCaps{})
	done := make(chan struct{})
	go func() { <-ctx.Done(); r.AddIn(3); r.End(); h.live.remove(e); close(done) }()
	deadline, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if err := h.Shutdown(deadline); err != nil {
		t.Fatal(err)
	}
	<-done
	rows, _ := m.Ledger("ns")
	if rows[0].BytesIn != 3 || h.live.tryAdd(&liveEntry{}, LiveCaps{}) {
		t.Fatal(rows)
	}
}
func TestShutdownRequestIsNotAdmittedOrMetered(t *testing.T) {
	h, srv, _, cookie := liveFixture(t, http.NotFoundHandler())
	m := NewMeter("b", time.Now())
	h.WithAccounting(m, func(string, string) (string, bool) { return "lab", true })
	_ = h.Shutdown(context.Background())
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "web-abc123.challenges.example.com"
	req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal(resp.StatusCode)
	}
	rows, _ := m.Ledger(GroupNamespace("g1"))
	if len(rows) != 0 {
		t.Fatal(rows)
	}
}

func TestFailedPreparationDoesNotAdmitOrCount(t *testing.T) {
	h, srv, _, cookie := liveFixture(t, http.NotFoundHandler())
	m := NewMeter("b", time.Now())
	h.WithAccounting(m, func(string, string) (string, bool) { return "lab", true }).WithReportPreparation(func(context.Context, string) error { return errors.New("restore unavailable") })
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "web-abc123.challenges.example.com"
	req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	rows, _ := m.Ledger(GroupNamespace("g1"))
	if resp.StatusCode != 503 || len(rows) != 0 || len(h.live.snapshot()) != 0 {
		t.Fatal(resp.StatusCode, rows)
	}
}
