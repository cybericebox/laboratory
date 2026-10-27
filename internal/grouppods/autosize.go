package grouppods

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// Autosize is how the pods a group runs itself (the VPN and the gateway) are sized after they are created. The operator, alone,
// watches the load of the group (VPN clients; labs that use the internet) and resizes the live pod in place (no restart, WireGuard
// sessions and NAT stay): memory request = limit = Sizing.Base + Sizing.PerUnit * units (so that the node's own accounting
// guarantees it and the pod never uses more than it requested, which is what keeps it last in line for eviction), CPU limit =
// the same formula over a fixed CPU request (CPU is compressible: no eviction, a burst is capped by the limit). Never above the
// maximum of the sizing (Sizing.Max()), never below what the pod was created with. Limits are rounded up to a step so that one more
// client does not mean one more resize, grow at once, and only the CPU shrinks, and only after the load has been lower for
// ShrinkAfter; the memory of a live pod is never lowered.
type Autosize struct {
	// Interval is how often the operator looks at the load of every autosizing group (GROUP_AUTOSIZE_INTERVAL).
	Interval time.Duration `env:"GROUP_AUTOSIZE_INTERVAL" envDefault:"15s"`
	// ShrinkAfter is how long a lower CPU need must last before the CPU limit goes down (GROUP_AUTOSIZE_SHRINK_AFTER).
	ShrinkAfter time.Duration `env:"GROUP_AUTOSIZE_SHRINK_AFTER" envDefault:"30m"`
	// HeadroomCPU is the least the CPU limit sits above the request (the pod is Burstable by it; the kubelet resizes only a pod
	// whose QoS class stays the same).
	HeadroomCPU string `env:"GROUP_AUTOSIZE_HEADROOM_CPU" envDefault:"10m"`
}

// Steps the limits are rounded up to.
const (
	StepCPU    = int64(5)
	StepMemory = int64(8 << 20)
)

// Headroom is the parsed CPU headroom in millicores (the default for an empty or bad value).
func (a Autosize) Headroom() int64 {
	if q, err := resource.ParseQuantity(a.HeadroomCPU); err == nil && q.Sign() > 0 {
		return q.MilliValue()
	}
	return 10
}

// Validate checks the headroom and the intervals.
func (a Autosize) Validate() error {
	if a.HeadroomCPU != "" {
		if q, err := resource.ParseQuantity(a.HeadroomCPU); err != nil || q.Sign() <= 0 {
			return fmt.Errorf("GROUP_AUTOSIZE_HEADROOM_CPU %q is not a positive quantity", a.HeadroomCPU)
		}
	}
	if a.Interval < 0 || a.ShrinkAfter < 0 {
		return fmt.Errorf("GROUP_AUTOSIZE_INTERVAL and GROUP_AUTOSIZE_SHRINK_AFTER cannot be negative")
	}
	return nil
}

func roundUp(v, step int64) int64 {
	if r := v % step; r != 0 {
		return v + step - r
	}
	return v
}

func limitDimension(want, request, maximum, headroom, step int64) int64 {
	if maximum <= request {
		return request
	}
	lo := request + headroom
	if lo > maximum {
		lo = maximum
	}
	if want < lo {
		want = lo
	}
	want = roundUp(want, step)
	if want > maximum {
		want = maximum
	}
	return want
}

// Limit is the size of a pod with the given requests for the given load: the CPU limit (at least headroomCPU above the CPU request)
// and the memory (request = limit, at least the request), both at most `maximum`.
func Limit(s PodSizing, request, maximum Overhead, units int, headroomCPU int64) Overhead {
	n := int64(units)
	return Overhead{
		CPU:    limitDimension(s.BaseCPU+s.PerUnitCPU*n, request.CPU, maximum.CPU, headroomCPU, StepCPU),
		Memory: limitDimension(s.BaseMemory+s.PerUnitMemory*n, request.Memory, maximum.Memory, 0, StepMemory),
	}
}

// SizingOf is the sizing of the named pod.
func (c Config) SizingOf(component string) (PodSizing, error) {
	all, err := c.Sizing.withDefaults().Parse()
	if err != nil {
		return PodSizing{}, err
	}
	if component == names.ComponentVPN {
		return all.VPN, nil
	}
	return all.Gateway, nil
}

// PodFor is the resources of the named pod's container when it is created. A pod the platform sized (or the chart's default) is
// created in the shape the autosizer can resize in place: CPU request = the size, CPU limit above it by the headroom, memory
// request = limit = the size.
func (c Config) PodFor(component string, size *laboratoryv1alpha1.GroupPodSize, units int) corev1.ResourceRequirements {
	var fixed corev1.ResourceRequirements
	if component == names.ComponentVPN {
		fixed = c.VPNFor(size)
	} else {
		fixed = c.GatewayFor(size)
	}
	sizing, err := c.SizingOf(component)
	if err != nil {
		return fixed
	}
	req := Overhead{CPU: fixed.Requests.Cpu().MilliValue(), Memory: fixed.Requests.Memory().Value()}
	lim := Limit(sizing, req, sizing.Max(), units, c.Autosize.Headroom())
	// The memory stays where the pod was created (a pod is created for the load it has, and grows from there).
	lim.Memory = req.Memory
	return corev1.ResourceRequirements{
		Requests: fixed.Requests.DeepCopy(),
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(lim.CPU, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(lim.Memory, resource.BinarySI),
		},
	}
}

// withDefaults fills what a Config built without the environment (a test, a reconciler built by hand) left empty with the
// defaults of the env tags.
func (s Sizing) withDefaults() Sizing {
	str := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	str(&s.VPNBaseCPU, "20m")
	str(&s.VPNBaseMemory, "64Mi")
	str(&s.VPNPerUserCPU, "6m")
	str(&s.VPNPerUserMemory, "20Mi")
	str(&s.GatewayBaseCPU, "5m")
	str(&s.GatewayBaseMemory, "16Mi")
	str(&s.GatewayPerLabCPU, "2m")
	str(&s.GatewayPerLabMemory, "4Mi")
	if s.VPNMaxUsers == 0 {
		s.VPNMaxUsers = 20
	}
	if s.GatewayMaxLabs == 0 {
		s.GatewayMaxLabs = 50
	}
	return s
}
