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

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
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
	HostPath string // the device on the host as the kubelet sees it (what a container gets); default TUNDevice
	// CheckPath is what THIS process looks at to see that the device exists on the host. A container's /dev is minimal and has no tun
	// even when the host does, so the node-agent checks the kernel's sysfs entry of the tun misc device, /sys/class/misc/tun/dev
	// (present exactly when the tun module is loaded; sysfs shows the host's kernel in a container, read-only, no extra mount).
	// Any path there only has to exist. Empty: HostPath, which must then be a character device.
	CheckPath string
	Interval  time.Duration
	Log       logr.Logger

	mu      sync.Mutex
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

// SetHostPath changes the device the plugin looks for (tests move it); the check path follows unless one was set.
func (p *Plugin) SetHostPath(path string) {
	p.mu.Lock()
	p.HostPath = path
	p.mu.Unlock()
}

// SetCheckPath changes where the plugin checks the device exists (tests move it).
func (p *Plugin) SetCheckPath(path string) {
	p.mu.Lock()
	p.CheckPath = path
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
	path := p.hostPath()
	p.mu.Lock()
	if p.CheckPath != "" {
		path = p.CheckPath
	}
	p.mu.Unlock()
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	p.mu.Lock()
	viaCheck := p.CheckPath != ""
	p.mu.Unlock()
	return viaCheck || st.Mode()&os.ModeCharDevice != 0
}

func (p *Plugin) checkPath() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.CheckPath != "" {
		return p.CheckPath
	}
	if p.HostPath != "" {
		return p.HostPath
	}
	return TUNDevice
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
	p.Log.Info("device plugin: advertising", "resource", p.Resource, "slots", p.Slots, "deviceOnHost", last, "checkPath", p.checkPath())
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
				p.Log.Info("device plugin: health changed", "resource", p.Resource, "deviceOnHost", now, "checkPath", p.checkPath())
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

// served is one run of the gRPC server on the plugin socket.
type served struct {
	srv  *grpc.Server
	sock string
	info os.FileInfo // the socket file as created, to tell that it was replaced or removed
}

func (p *Plugin) startServer() (*served, error) {
	sock := filepath.Join(p.Dir, p.socketName())
	_ = os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", sock, err)
	}
	st, err := os.Lstat(sock)
	if err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("stat %s: %w", sock, err)
	}
	srv := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(srv, p)
	go func() { _ = srv.Serve(lis) }()
	return &served{srv: srv, sock: sock, info: st}, nil
}

// stop ends the server (its listener and open streams) and removes the socket file if it is still ours.
func (s *served) stop() {
	s.srv.Stop()
	if st, err := os.Lstat(s.sock); err == nil && os.SameFile(st, s.info) {
		_ = os.Remove(s.sock)
	}
}

// lost says whether the socket file is gone or is no longer the one this server listens on.
func (s *served) lost() bool {
	st, err := os.Lstat(s.sock)
	return err != nil || !os.SameFile(st, s.info)
}

// run is the state of one Serve call: the server, and whether the kubelet has our registration.
type run struct {
	p              *Plugin
	cur            *served
	kubeletSock    string
	registeredWith os.FileInfo // the kubelet.sock registered with; nil: registration is due
	backoff        time.Duration
	retry          *time.Timer
	waiting        bool
}

const maxBackoff = 30 * time.Second

// restart serves again on a fresh socket and makes the registration due.
func (r *run) restart(why string) error {
	r.p.Log.Info("device plugin: serving again", "reason", why)
	r.cur.stop()
	next, err := r.p.startServer()
	if err != nil {
		return err
	}
	r.cur = next
	r.registeredWith = nil
	r.backoff = 0
	resetTimer(r.retry, 0)
	return nil
}

func (r *run) onEvent(ev fsnotify.Event) error {
	switch {
	case ev.Name == r.kubeletSock && ev.Has(fsnotify.Create) && r.registeredWith == nil && !r.cur.lost():
		// Not registered yet (the first start; some backends also report files that already exist): just register now.
		resetTimer(r.retry, 0)
	case ev.Name == r.kubeletSock && ev.Has(fsnotify.Create) && sameFile(r.kubeletSock, r.registeredWith):
		// a replayed event for the socket we registered with
	case ev.Name == r.kubeletSock && ev.Has(fsnotify.Create):
		return r.restart("the kubelet socket was created")
	case ev.Name == r.cur.sock && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) && r.cur.lost():
		// (lost: the removal made by our own restart is already followed by a new socket and must not loop)
		return r.restart("the plugin socket was removed")
	}
	return nil
}

// onPoll is the slow check that covers lost events (and platforms whose watch cannot see a socket file go).
func (r *run) onPoll() error {
	if r.cur.lost() {
		return r.restart("the plugin socket is gone")
	}
	if st, err := os.Stat(r.kubeletSock); err == nil && r.registeredWith != nil && !same(st, r.registeredWith) {
		return r.restart("the kubelet socket was replaced")
	}
	return nil
}

// onRetry registers when it is due, and schedules the next try (growing delay) when the kubelet does not accept.
func (r *run) onRetry(ctx context.Context) {
	st, err := os.Stat(r.kubeletSock)
	if err != nil {
		if !r.waiting {
			r.p.Log.Info("device plugin: waiting for the kubelet socket", "socket", r.kubeletSock)
			r.waiting = true
		}
		return // the Create event (or the poll) brings it back
	}
	r.waiting = false
	if r.registeredWith != nil && same(st, r.registeredWith) {
		return
	}
	if err := r.p.register(ctx, r.kubeletSock); err != nil {
		if r.backoff == 0 {
			r.backoff = 500 * time.Millisecond
		} else if r.backoff *= 2; r.backoff > maxBackoff {
			r.backoff = maxBackoff
		}
		r.p.Log.Error(err, "device plugin: registration with the kubelet failed (will retry)", "socket", r.kubeletSock, "in", r.backoff.String())
		resetTimer(r.retry, r.backoff)
		return
	}
	r.registeredWith = st
	r.backoff = 0
	r.p.Log.Info("device plugin: registered with the kubelet", "resource", r.p.Resource, "endpoint", r.p.socketName())
}

// Serve listens on the plugin's socket in the kubelet directory and registers with the kubelet. A restarted kubelet forgets
// every plugin, empties the directory (our socket goes with it) and creates a new kubelet.sock, so the plugin watches the
// directory: when kubelet.sock is created, or our own socket is removed or replaced, it restarts the server on a fresh socket
// and registers again, retrying with a growing delay until the kubelet accepts. A slow poll covers lost events (a recreated
// socket may reuse the inode). It returns when ctx ends, or at once if the directory cannot be used.
func (p *Plugin) Serve(ctx context.Context) error {
	if p.Log.GetSink() == nil {
		p.Log = logr.Discard()
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch %s: %w", p.Dir, err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Add(p.Dir); err != nil {
		return fmt.Errorf("watch %s: %w", p.Dir, err)
	}
	cur, err := p.startServer()
	if err != nil {
		return err
	}
	r := &run{p: p, cur: cur, kubeletSock: filepath.Join(p.Dir, "kubelet.sock"), retry: time.NewTimer(0)}
	defer r.retry.Stop()
	defer func() { r.cur.stop() }()
	poll := time.NewTicker(p.interval())
	defer poll.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return fmt.Errorf("watch %s: closed", p.Dir)
			}
			err = r.onEvent(ev)
		case werr, ok := <-w.Errors:
			if !ok {
				return fmt.Errorf("watch %s: closed", p.Dir)
			}
			p.Log.Error(werr, "device plugin: watching the directory failed", "dir", p.Dir)
		case <-poll.C:
			err = r.onPoll()
		case <-r.retry.C:
			r.onRetry(ctx)
		}
		if err != nil {
			return err
		}
	}
}

// same says whether two stats are the same socket file. The inode alone is not enough: a recreated socket often gets the
// inode of the one just removed, so the modification time (the moment of the bind) must match too.
func same(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime())
}

func sameFile(path string, prev os.FileInfo) bool {
	if prev == nil {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && same(st, prev)
}

// resetTimer re-arms a timer that may have fired or not.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func (p *Plugin) register(ctx context.Context, kubeletSock string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("unix://"+kubeletSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = pluginapi.NewRegistrationClient(conn).Register(ctx, &pluginapi.RegisterRequest{
		Version: pluginapi.Version, Endpoint: p.socketName(), ResourceName: p.Resource,
	})
	return err
}
