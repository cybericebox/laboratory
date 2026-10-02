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
	auths               map[string]*Auth
	inflight, maxFlight int
	delay               time.Duration
}

func (f *fakePuller) Present(_ context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.have[ref], nil
}

func (f *fakePuller) Pull(_ context.Context, ref string, auth *Auth) error {
	f.mu.Lock()
	f.inflight++
	if f.inflight > f.maxFlight {
		f.maxFlight = f.inflight
	}
	f.pulls = append(f.pulls, ref)
	if f.auths == nil {
		f.auths = map[string]*Auth{}
	}
	f.auths[ref] = auth
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

func TestPullUsesTheCredentialOfEachImageAndKeepsItOutOfMessages(t *testing.T) {
	p := &fakePuller{fail: map[string]error{"ghcr.io/o/private:1": errors.New("denied for hunter2hunter2 at ghcr.io")}}
	res := Pull(context.Background(), p, []string{"ghcr.io/o/private:1", "docker.io/library/nginx"}, Options{
		Credentials: func(ref string) *Auth {
			if strings.HasPrefix(ref, "ghcr.io/") {
				return &Auth{Username: "tenant", Password: "hunter2hunter2"}
			}
			return nil
		},
	})
	if a := p.auths["ghcr.io/o/private:1"]; a == nil || a.Password != "hunter2hunter2" {
		t.Fatalf("auth: %+v", a)
	}
	if p.auths["docker.io/library/nginx"] != nil {
		t.Fatal("anonymous for a registry without credentials")
	}
	if len(res.Failed) != 1 || strings.Contains(res.Failed[0].Message, "hunter2") {
		t.Fatalf("the credential must not leak into the message: %+v", res.Failed)
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
func (blockingPuller) Pull(ctx context.Context, _ string, _ *Auth) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestDockerConfigCredentials(t *testing.T) {
	doc := []byte(`{"auths":{"ghcr.io":{"username":"u","password":"pppp"},"https://index.docker.io/v1/":{"auth":"aGI6c2VjcmV0"}}}`)
	cred, err := DockerConfigCredentials(doc)
	if err != nil {
		t.Fatal(err)
	}
	if a := cred("ghcr.io/o/x:1"); a == nil || a.Username != "u" || a.Password != "pppp" {
		t.Fatalf("ghcr: %+v", a)
	}
	if a := cred("nginx"); a == nil || a.Username != "hb" || a.Password != "secret" {
		t.Fatalf("docker hub: %+v", a)
	}
	if cred("quay.io/o/x:1") != nil || cred("NOT A REF") != nil {
		t.Fatal("no entry, no credential")
	}
	none, _ := DockerConfigCredentials(nil)
	if none("ghcr.io/o/x:1") != nil {
		t.Fatal("an empty document is anonymous")
	}
	if _, err := DockerConfigCredentials([]byte("{")); err == nil {
		t.Fatal("a malformed document is an error")
	}
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
	res := Pull(context.Background(), c, []string{"ghcr.io/o/held:1", "ghcr.io/o/new:1", "ghcr.io/o/broken:1"}, Options{
		Credentials: func(string) *Auth { return &Auth{Username: "u", Password: "p"} },
	})
	if strings.Join(res.Pulled, ",") != "ghcr.io/o/held:1,ghcr.io/o/new:1" || len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Message, "manifest unknown") {
		t.Fatalf("%+v", res)
	}
	if a := f.auth["ghcr.io/o/new:1"]; a == nil || a.Username != "u" || a.Password != "p" {
		t.Fatalf("the runtime got the credential: %+v", a)
	}
	if _, ok := f.auth["ghcr.io/o/held:1"]; ok {
		t.Fatal("a held image is not pulled again")
	}
}
