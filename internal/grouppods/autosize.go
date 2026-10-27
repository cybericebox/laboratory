package grouppods

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// Autosize is how a group pod with a MaxSize is sized. The pod keeps its Size as the request (the minimum the platform plans
// with) and its limits follow the load of the group: limit = Sizing.Base + Sizing.PerUnit * units (the same base and per-unit
// values the platform sizes groups with: VPN_BASE_CPU, VPN_PER_USER_CPU, ..., GATEWAY_PER_LAB_...), never below the request plus
// the headroom (the limit stays above the request, so the pod stays Burstable and the kubelet can resize it in place) and never
// above MaxSize. Limits are rounded up to a step so that one more client does not mean one more resize, grow at once, and shrink
// only the CPU, and only after the load has been lower for ShrinkAfter. Memory is never lowered on a live pod.
type Autosize struct {
	// Interval is how often the operator looks at the load of every autosizing group (GROUP_AUTOSIZE_INTERVAL).
	Interval time.Duration `env:"GROUP_AUTOSIZE_INTERVAL" envDefault:"15s"`
	// ShrinkAfter is how long a lower CPU need must last before the CPU limit goes down (GROUP_AUTOSIZE_SHRINK_AFTER).
	ShrinkAfter time.Duration `env:"GROUP_AUTOSIZE_SHRINK_AFTER" envDefault:"30m"`
	// HeadroomCPU and HeadroomMemory are the least the limit sits above the request.
	HeadroomCPU    string `env:"GROUP_AUTOSIZE_HEADROOM_CPU" envDefault:"10m"`
	HeadroomMemory string `env:"GROUP_AUTOSIZE_HEADROOM_MEMORY" envDefault:"16Mi"`
}

// Steps the limits are rounded up to.
const (
	StepCPU    = int64(5)
	StepMemory = int64(8 << 20)
)

// Headroom is the parsed headroom (the defaults for an empty or bad value).
func (a Autosize) Headroom() Overhead {
	h := Overhead{CPU: 10, Memory: 16 << 20}
	if q, err := resource.ParseQuantity(a.HeadroomCPU); err == nil && q.Sign() > 0 {
		h.CPU = q.MilliValue()
	}
	if q, err := resource.ParseQuantity(a.HeadroomMemory); err == nil && q.Sign() > 0 {
		h.Memory = q.Value()
	}
	return h
}

// Validate checks the headroom and the intervals.
func (a Autosize) Validate() error {
	for name, v := range map[string]string{"GROUP_AUTOSIZE_HEADROOM_CPU": a.HeadroomCPU, "GROUP_AUTOSIZE_HEADROOM_MEMORY": a.HeadroomMemory} {
		if v == "" {
			continue
		}
		if q, err := resource.ParseQuantity(v); err != nil || q.Sign() <= 0 {
			return fmt.Errorf("%s %q is not a positive quantity", name, v)
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

// Limit is the limits of a pod with the given requests and maximum for the given load.
func Limit(s PodSizing, request, maximum Overhead, units int, headroom Overhead) Overhead {
	n := int64(units)
	return Overhead{
		CPU:    limitDimension(s.BaseCPU+s.PerUnitCPU*n, request.CPU, maximum.CPU, headroom.CPU, StepCPU),
		Memory: limitDimension(s.BaseMemory+s.PerUnitMemory*n, request.Memory, maximum.Memory, headroom.Memory, StepMemory),
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

// MaxSizeOf is the autosizing maximum of the named pod in the group's spec; nil when the pod is fixed at its size.
func MaxSizeOf(component string, lg *laboratoryv1alpha1.LabGroup) *laboratoryv1alpha1.GroupPodSize {
	if lg == nil {
		return nil
	}
	if component == names.ComponentVPN {
		return lg.Spec.VPN.MaxSize
	}
	return lg.Spec.Gateway.MaxSize
}

// SizeOf is the size (the request) of the named pod in the group's spec, nil for the chart's default.
func SizeOf(component string, lg *laboratoryv1alpha1.LabGroup) *laboratoryv1alpha1.GroupPodSize {
	if lg == nil {
		return nil
	}
	if component == names.ComponentVPN {
		return lg.Spec.VPN.Size
	}
	return lg.Spec.Gateway.Size
}

// PodFor is the resources of the named pod's container when it is created: the fixed size (requests = limits) without a max size;
// with one, the requests are the size and the limits are the limits for `units` of load.
func (c Config) PodFor(component string, size, maximum *laboratoryv1alpha1.GroupPodSize, units int) corev1.ResourceRequirements {
	var fixed corev1.ResourceRequirements
	if component == names.ComponentVPN {
		fixed = c.VPNFor(size)
	} else {
		fixed = c.GatewayFor(size)
	}
	if maximum == nil {
		return fixed
	}
	sizing, err := c.SizingOf(component)
	if err != nil {
		return fixed
	}
	req := Overhead{CPU: fixed.Requests.Cpu().MilliValue(), Memory: fixed.Requests.Memory().Value()}
	lim := Limit(sizing, req, Overhead{CPU: maximum.CPUMillicores, Memory: maximum.MemoryBytes}, units, c.Autosize.Headroom())
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
