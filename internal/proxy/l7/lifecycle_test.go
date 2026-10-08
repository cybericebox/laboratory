package l7

import (
	"context"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"io"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"net"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestLifecycleSettlesActiveStreamBeforeFinalReport(t *testing.T) {
	h, _, _, cookie := liveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&lab.LabTrafficReport{}).Build()
	m := NewMeter("b", time.Now())
	h.WithAccounting(m, func(string, string) (string, bool) { return "lab", true })
	writer := &ReportWriter{Reader: store, Writer: store, Meter: m, Instance: "pod", Namespaces: func(context.Context) []string { return []string{GroupNamespace("g1")} }}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunLifecycle(ctx, server, h, writer, time.Hour, time.Hour, func() error { return server.Serve(ln) }, nil)
	}()
	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/", nil)
	req.Host = "web-abc123.challenges.example.com"
	req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err = io.ReadFull(resp.Body, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle did not stop")
	}
	var report lab.LabTrafficReport
	if err = store.Get(context.Background(), types.NamespacedName{Namespace: GroupNamespace("g1"), Name: ReportName("pod")}, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Status.Ledger) != 1 || report.Status.Ledger[0].BytesIn != 5 || len(h.live.snapshot()) != 0 {
		t.Fatal(report.Status)
	}
}
