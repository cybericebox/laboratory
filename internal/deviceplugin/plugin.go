// Package deviceplugin is the kubelet device plugin of the node-agent. It advertises one extended resource,
// cybericebox.com/tun, and a pod that requests it gets /dev/net/tun passed by the kubelet and nothing else from the
// host: that is how the `extended` device profile reaches tun without a privileged container or a hostPath.
package deviceplugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// TUNDevice is the host path of the tun device the plugin hands out; the container sees it at the same path.
const TUNDevice = "/dev/net/tun"

// Plugin serves one resource. Its devices are interchangeable virtual slots (the same /dev/net/tun for each), so a node
// can run as many extended devices as it has slots; the count only bounds that.
type Plugin struct {
	pluginapi.UnimplementedDevicePluginServer

	Resource string // cybericebox.com/tun
	Dir      string // the kubelet's device-plugins directory
	Slots    int
	HostPath string // the device on the host; default TUNDevice
	Interval time.Duration

	mu      sync.Mutex
	healthy bool
	changed chan struct{}
}

func (p *Plugin) hostPath() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.HostPath != "" {
		return p.HostPath
	}
	return TUNDevice
}

// SetHostPath changes the device the plugin looks for (tests move it).
func (p *Plugin) SetHostPath(path string) {
	p.mu.Lock()
	p.HostPath = path
	p.mu.Unlock()
}

func (p *Plugin) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return 10 * time.Second
}

func (p *Plugin) socketName() string { return "cybericebox-tun.sock" }

// present says whether the device exists on the host (the tun module is loaded).
func (p *Plugin) present() bool {
	st, err := os.Stat(p.hostPath())
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (p *Plugin) devices(healthy bool) []*pluginapi.Device {
	state := pluginapi.Healthy
	if !healthy {
		state = pluginapi.Unhealthy
	}
	out := make([]*pluginapi.Device, p.Slots)
	for i := range out {
		out[i] = &pluginapi.Device{ID: fmt.Sprintf("tun-%d", i), Health: state}
	}
	return out
}

func (p *Plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch sends the devices, and again whenever the health of the host device changes (it appears or goes away).
func (p *Plugin) ListAndWatch(_ *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	p.mu.Lock()
	if p.changed == nil {
		p.changed = make(chan struct{}, 1)
	}
	p.mu.Unlock()
	last := p.present()
	if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: p.devices(last)}); err != nil {
		return err
	}
	t := time.NewTicker(p.interval())
	defer t.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-t.C:
			if now := p.present(); now != last {
				last = now
				if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: p.devices(now)}); err != nil {
					return err
				}
			}
		}
	}
}

// Allocate answers with the tun device for every container request: that is all the kubelet passes.
func (p *Plugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	if !p.present() {
		return nil, fmt.Errorf("%s is not on this node (the tun module is not loaded)", p.hostPath())
	}
	resp := &pluginapi.AllocateResponse{}
	for range req.ContainerRequests {
		resp.ContainerResponses = append(resp.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			Devices: []*pluginapi.DeviceSpec{{ContainerPath: TUNDevice, HostPath: p.hostPath(), Permissions: "rwm"}},
		})
	}
	return resp, nil
}

func (p *Plugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (p *Plugin) GetPreferredAllocation(context.Context, *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// Serve listens on the plugin's socket in the kubelet directory and registers with the kubelet; it registers again
// when the kubelet restarts (its socket is replaced). It returns when ctx ends.
func (p *Plugin) Serve(ctx context.Context) error {
	sock := filepath.Join(p.Dir, p.socketName())
	_ = os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sock, err)
	}
	srv := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(srv, p)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	defer os.Remove(sock)

	kubeletSock := filepath.Join(p.Dir, "kubelet.sock")
	var registeredWith os.FileInfo
	for {
		// The kubelet's socket is a new file after a restart: register with the new one.
		if st, err := os.Stat(kubeletSock); err == nil && (registeredWith == nil || !os.SameFile(st, registeredWith)) {
			if err := p.register(ctx, kubeletSock); err == nil {
				registeredWith = st
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(p.interval()):
		}
	}
}

func (p *Plugin) register(ctx context.Context, kubeletSock string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("unix://"+kubeletSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = pluginapi.NewRegistrationClient(conn).Register(ctx, &pluginapi.RegisterRequest{
		Version: pluginapi.Version, Endpoint: p.socketName(), ResourceName: p.Resource,
	})
	return err
}
