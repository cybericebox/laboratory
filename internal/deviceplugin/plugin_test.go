package deviceplugin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// shortDir is a short temporary directory: unix socket paths are limited to about 100 bytes.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// fakeKubelet is a registration server that records what registers with it.
type fakeKubelet struct {
	pluginapi.UnimplementedRegistrationServer
	got chan *pluginapi.RegisterRequest
}

func (f *fakeKubelet) Register(_ context.Context, r *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	f.got <- r
	return &pluginapi.Empty{}, nil
}

func startKubelet(t *testing.T, dir string) *fakeKubelet {
	t.Helper()
	lis, err := net.Listen("unix", filepath.Join(dir, "kubelet.sock"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKubelet{got: make(chan *pluginapi.RegisterRequest, 4)}
	srv := grpc.NewServer()
	pluginapi.RegisterRegistrationServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return f
}

// A "char device" stand-in: /dev/null is a character device on every unix.
func newPlugin(dir string) *Plugin {
	return &Plugin{Resource: "cybericebox.com/tun", Dir: dir, Slots: 3, HostPath: "/dev/null", Interval: 50 * time.Millisecond}
}

func TestAllocatePassesOnlyTheTunDevice(t *testing.T) {
	p := newPlugin(shortDir(t))
	resp, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"tun-0"}}, {DevicesIds: []string{"tun-1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ContainerResponses) != 2 {
		t.Fatalf("responses = %d", len(resp.ContainerResponses))
	}
	for _, c := range resp.ContainerResponses {
		if len(c.Devices) != 1 || c.Devices[0].ContainerPath != "/dev/net/tun" || c.Devices[0].HostPath != "/dev/null" || len(c.Mounts) != 0 || len(c.Envs) != 0 {
			t.Fatalf("a container gets only the tun device: %+v", c)
		}
	}
	// without the device on the host nothing is allocated
	p.SetHostPath(filepath.Join(shortDir(t), "absent"))
	if _, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{{}}}); err == nil {
		t.Fatal("a missing device must fail the allocation")
	}
}

func TestRegistersAndAdvertisesTheSlots(t *testing.T) {
	dir := shortDir(t)
	kubelet := startKubelet(t, dir)
	p := newPlugin(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Serve(ctx) }()

	select {
	case r := <-kubelet.got:
		if r.ResourceName != "cybericebox.com/tun" || r.Version != pluginapi.Version || r.Endpoint != "cybericebox-tun.sock" {
			t.Fatalf("registration: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the plugin did not register")
	}

	conn, err := grpc.NewClient("unix://"+filepath.Join(dir, "cybericebox-tun.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := pluginapi.NewDevicePluginClient(conn).ListAndWatch(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || len(first.Devices) != 3 || first.Devices[0].Health != pluginapi.Healthy {
		t.Fatalf("devices: %+v %v", first, err)
	}
}

// When the host device appears or goes, the advertised health follows.
func TestHealthFollowsTheHostDevice(t *testing.T) {
	dir := shortDir(t)
	p := newPlugin(dir)
	p.SetHostPath(filepath.Join(dir, "absent")) // not a char device: unhealthy
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "cybericebox-tun.sock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, _ := grpc.NewClient("unix://"+filepath.Join(dir, "cybericebox-tun.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()
	stream, err := pluginapi.NewDevicePluginClient(conn).ListAndWatch(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.Devices[0].Health != pluginapi.Unhealthy {
		t.Fatalf("without the device the slots are unhealthy: %+v %v", first, err)
	}
	p.SetHostPath("/dev/null") // the device appears
	next, err := stream.Recv()
	if err != nil || next.Devices[0].Health != pluginapi.Healthy {
		t.Fatalf("after it appears the slots are healthy: %+v %v", next, err)
	}
}
