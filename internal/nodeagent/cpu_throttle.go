//go:build linux

package nodeagent

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/cputhrottle"
	"github.com/cybericebox/laboratory/internal/devicestate"
)

// cpuThrottleInterval is how often the cpu.stat of the device containers is read.
const cpuThrottleInterval = 30 * time.Second

// CPUThrottleMonitor reports how much the CPU limit holds a device back: every cpuThrottleInterval it reads the CFS counters
// (cpu.stat) of the device containers of its node, keeps the last few minutes of them, and writes the share of throttled
// periods to Device.status.cpuThrottle when it changes by a step. The lab controller copies it to the lab status, the agent
// sends it on (LabDeviceStatus.cpu_throttled_percent), and the platform alerts on it.
type CPUThrottleMonitor struct {
	Client     client.Client
	NodeName   string
	CgroupRoot string
	Interval   time.Duration

	now     func() time.Time
	windows map[string]*cputhrottle.Window
	dirs    map[string]string
}

// NeedLeaderElection: every node-agent looks after the pods of its own node.
func (m *CPUThrottleMonitor) NeedLeaderElection() bool { return false }

// Start blocks until ctx ends.
func (m *CPUThrottleMonitor) Start(ctx context.Context) error {
	interval := m.Interval
	if interval <= 0 {
		interval = cpuThrottleInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		m.Check(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Check runs one round over the devices of the node.
func (m *CPUThrottleMonitor) Check(ctx context.Context) {
	log := ctrl.Log.WithName("cpu-throttle")
	if m.now == nil {
		m.now = time.Now
	}
	if m.windows == nil {
		m.windows, m.dirs = map[string]*cputhrottle.Window{}, map[string]string{}
	}
	var devices lab.DeviceList
	if err := m.Client.List(ctx, &devices); err != nil {
		log.Error(err, "list devices")
		return
	}
	seen := map[string]bool{}
	for i := range devices.Items {
		d := &devices.Items[i]
		if d.Status.NodeName != m.NodeName || d.Status.PodName == "" || d.DeletionTimestamp != nil {
			continue
		}
		var pod corev1.Pod
		if err := m.Client.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: d.Status.PodName}, &pod); err != nil {
			continue
		}
		id := runningContainerID(&pod, d.Spec.Name)
		if id == "" {
			continue
		}
		seen[id] = true
		percent, covered, ok := m.sample(id)
		next, change := nextCPUThrottle(d.Status.CPUThrottle, percent, covered, ok, metav1.NewTime(m.now()))
		if !change {
			continue
		}
		base := d.DeepCopy()
		d.Status.CPUThrottle = next
		if err := m.Client.Status().Patch(ctx, d, client.MergeFrom(base)); err != nil {
			log.Error(err, "publish cpu throttle", "device", client.ObjectKeyFromObject(d))
		}
	}
	for id := range m.windows {
		if !seen[id] {
			delete(m.windows, id)
			delete(m.dirs, id)
		}
	}
}

// sample reads the counters of a container and returns its throttled share over the window.
func (m *CPUThrottleMonitor) sample(id string) (percent int32, covered time.Duration, ok bool) {
	dir := m.dirs[id]
	if dir == "" {
		if dir = devicestate.FindCgroup(m.CgroupRoot, id); dir == "" {
			return 0, 0, false
		}
		m.dirs[id] = dir
	}
	stat, err := cputhrottle.ReadStat(dir)
	if err != nil {
		// The cgroup is gone (the container was replaced) or has no limit: look again next round.
		delete(m.dirs, id)
		delete(m.windows, id)
		return 0, 0, false
	}
	w := m.windows[id]
	if w == nil {
		w = &cputhrottle.Window{}
		m.windows[id] = w
	}
	w.Add(m.now(), stat)
	return w.Percent()
}

// nextCPUThrottle decides what Device.status.cpuThrottle becomes. Not throttled (or no data: the container is idle, so the
// counters stand still) is nil; a value is written only when the percent changes.
func nextCPUThrottle(current *lab.DeviceCPUThrottle, percent int32, covered time.Duration, ok bool, now metav1.Time) (*lab.DeviceCPUThrottle, bool) {
	if !ok || percent == 0 {
		return nil, current != nil
	}
	if current != nil && current.Percent == percent {
		return current, false
	}
	return &lab.DeviceCPUThrottle{Percent: percent, WindowSeconds: int32(covered / time.Second), ObservedAt: now}, true
}

// runningContainerID is the id of the running container of the given name in the pod ("containerd://<id>" without the scheme).
func runningContainerID(pod *corev1.Pod, name string) string {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == name && s.State.Running != nil {
			_, id, _ := strings.Cut(s.ContainerID, "://")
			return id
		}
	}
	return ""
}
