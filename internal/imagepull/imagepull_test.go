package imagepull

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakePuller struct {
	mu                  sync.Mutex
	have                map[string]bool
	fail                map[string]error
	pulls               []string
	inflight, maxFlight int
	delay               time.Duration
}

func (f *fakePuller) Present(_ context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.have[ref], nil
}

func (f *fakePuller) Pull(_ context.Context, ref string) error {
	f.mu.Lock()
	f.inflight++
	if f.inflight > f.maxFlight {
		f.maxFlight = f.inflight
	}
	f.pulls = append(f.pulls, ref)
	err := f.fail[ref]
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	f.inflight--
	f.mu.Unlock()
	return err
}

func TestPullSkipsWhatTheNodeHoldsAndReportsFailures(t *testing.T) {
	p := &fakePuller{
		have: map[string]bool{"ghcr.io/o/held:1": true},
		fail: map[string]error{"ghcr.io/o/broken:1": errors.New("manifest unknown")},
	}
	res := Pull(context.Background(), p, []string{
		"ghcr.io/o/new:1", "ghcr.io/o/held:1", "ghcr.io/o/broken:1", "ghcr.io/o/new:1", "", "NOT A REF",
	}, Options{})
	if got := strings.Join(res.Pulled, ","); got != "ghcr.io/o/held:1,ghcr.io/o/new:1" {
		t.Fatalf("pulled: %s", got)
	}
	if len(res.Failed) != 2 || res.Failed[0].Image != "NOT A REF" || res.Failed[1].Image != "ghcr.io/o/broken:1" || res.Failed[1].Message != "manifest unknown" {
		t.Fatalf("failed: %+v", res.Failed)
	}
	if len(p.pulls) != 2 { // new, broken; held is not pulled, the invalid ref never reaches the runtime
		t.Fatalf("pulls: %v", p.pulls)
	}
}

func TestPullBoundsConcurrencyAndTime(t *testing.T) {
	p := &fakePuller{delay: 20 * time.Millisecond}
	var imgs []string
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		imgs = append(imgs, "ghcr.io/o/"+n+":1")
	}
	Pull(context.Background(), p, imgs, Options{Concurrency: 2})
	if p.maxFlight > 2 || p.maxFlight == 0 {
		t.Fatalf("at most 2 at once, saw %d", p.maxFlight)
	}

	slow := blockingPuller{}
	res := Pull(context.Background(), slow, []string{"ghcr.io/o/slow:1"}, Options{Timeout: 30 * time.Millisecond})
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Message, "deadline") {
		t.Fatalf("a hung pull times out: %+v", res)
	}
}

type blockingPuller struct{}

func (blockingPuller) Present(context.Context, string) (bool, error) { return false, nil }
func (blockingPuller) Pull(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// fakeCRI is the image service of a runtime.
type fakeCRI struct {
	criapi.UnimplementedImageServiceServer
	mu   sync.Mutex
	have map[string]bool
	auth map[string]*criapi.AuthConfig
	fail map[string]string
}

func (f *fakeCRI) ImageStatus(_ context.Context, r *criapi.ImageStatusRequest) (*criapi.ImageStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.have[r.GetImage().GetImage()] {
		return &criapi.ImageStatusResponse{Image: &criapi.Image{Id: "sha256:1"}}, nil
	}
	return &criapi.ImageStatusResponse{}, nil
}

func (f *fakeCRI) PullImage(_ context.Context, r *criapi.PullImageRequest) (*criapi.PullImageResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := r.GetImage().GetImage()
	if msg := f.fail[ref]; msg != "" {
		return nil, status.Error(codes.NotFound, msg)
	}
	f.have[ref] = true
	if f.auth == nil {
		f.auth = map[string]*criapi.AuthConfig{}
	}
	f.auth[ref] = r.GetAuth()
	return &criapi.PullImageResponse{ImageRef: "sha256:1"}, nil
}

func TestCRIPullerPullsThroughTheImageService(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "cri.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets: %v", err)
	}
	f := &fakeCRI{have: map[string]bool{"ghcr.io/o/held:1": true}, fail: map[string]string{"ghcr.io/o/broken:1": "manifest unknown"}}
	srv := grpc.NewServer()
	criapi.RegisterImageServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	c, err := NewCRIPuller(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res := Pull(context.Background(), c, []string{"ghcr.io/o/held:1", "ghcr.io/o/new:1", "ghcr.io/o/broken:1"}, Options{})
	if strings.Join(res.Pulled, ",") != "ghcr.io/o/held:1,ghcr.io/o/new:1" || len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Message, "manifest unknown") {
		t.Fatalf("%+v", res)
	}
	if a := f.auth["ghcr.io/o/new:1"]; a != nil {
		t.Fatalf("a pull carries no credential: %+v", a)
	}
	if _, ok := f.auth["ghcr.io/o/held:1"]; ok {
		t.Fatal("a held image is not pulled again")
	}
}

// Each image is timed by what became of it; no image name is a label.
func TestPullTimesEachImageByResult(t *testing.T) {
	before := func(r string) uint64 { return sampleCount(t, pullSeconds, "result", r) }
	present, pulled, failed := before("present"), before("pulled"), before("error")
	p := &fakePuller{have: map[string]bool{"ghcr.io/o/held:1": true}, fail: map[string]error{"ghcr.io/o/broken:1": errors.New("manifest unknown")}}
	Pull(context.Background(), p, []string{"ghcr.io/o/held:1", "ghcr.io/o/new:1", "ghcr.io/o/broken:1"}, Options{})
	if got := before("present") - present; got != 1 {
		t.Fatalf("present: %d", got)
	}
	if got := before("pulled") - pulled; got != 1 {
		t.Fatalf("pulled: %d", got)
	}
	if got := before("error") - failed; got != 1 {
		t.Fatalf("error: %d", got)
	}
}

func sampleCount(t *testing.T, c prometheus.Collector, labels ...string) uint64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			ok := true
			for i := 0; i+1 < len(labels); i += 2 {
				found := false
				for _, l := range m.GetLabel() {
					found = found || (l.GetName() == labels[i] && l.GetValue() == labels[i+1])
				}
				ok = ok && found
			}
			if ok {
				if h := m.GetHistogram(); h != nil {
					n += h.GetSampleCount()
				} else if c := m.GetCounter(); c != nil {
					n += uint64(c.GetValue())
				}
			}
		}
	}
	return n
}
