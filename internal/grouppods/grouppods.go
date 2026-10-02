// Package grouppods holds the resources of the pods a LabGroup runs itself, the VPN and the
// internet gateway. They are set once from the chart (requests = limits, so the pods are Guaranteed), by the
// operator when it creates a group and read by the management agent, which reports their sum as the service
// overhead of a group.
package grouppods

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Config is the chart's choice (env VPN_CPU, VPN_MEMORY, GATEWAY_CPU, GATEWAY_MEMORY, shared by the operator and
// the agent). The defaults come from measuring: an idle VPN pod about 1m and 16Mi, up to 55m and 195Mi with ten active
// peers (so 320Mi), a gateway about 1m and 8Mi: the data path is the kernel's WireGuard, the pod only manages peers and rules.
type Config struct {
	VPNCPU        string `env:"VPN_CPU" envDefault:"100m"`
	VPNMemory     string `env:"VPN_MEMORY" envDefault:"320Mi"`
	GatewayCPU    string `env:"GATEWAY_CPU" envDefault:"10m"`
	GatewayMemory string `env:"GATEWAY_MEMORY" envDefault:"32Mi"`
}

// Overhead is the CPU (millicores) and memory (bytes) a group's own pods request.
type Overhead struct {
	CPU, Memory int64
}

// Validate checks that every value is a positive quantity.
func (c Config) Validate() error {
	for name, v := range map[string]string{"VPN_CPU": c.VPNCPU, "VPN_MEMORY": c.VPNMemory, "GATEWAY_CPU": c.GatewayCPU, "GATEWAY_MEMORY": c.GatewayMemory} {
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("%s %q is not a positive quantity", name, v)
		}
	}
	return nil
}

// Defaults of the chart, used for a value left empty (a reconciler built without a config).
const (
	DefaultVPNCPU        = "100m"
	DefaultVPNMemory     = "320Mi"
	DefaultGatewayCPU    = "10m"
	DefaultGatewayMemory = "32Mi"
)

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func guaranteed(cpu, memory string) corev1.ResourceRequirements {
	l := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}
	return corev1.ResourceRequirements{Requests: l.DeepCopy(), Limits: l}
}

// VPN is the resources of a group's VPN container.
func (c Config) VPN() corev1.ResourceRequirements {
	return guaranteed(orDefault(c.VPNCPU, DefaultVPNCPU), orDefault(c.VPNMemory, DefaultVPNMemory))
}

// Gateway is the resources of a group's gateway container.
func (c Config) Gateway() corev1.ResourceRequirements {
	return guaranteed(orDefault(c.GatewayCPU, DefaultGatewayCPU), orDefault(c.GatewayMemory, DefaultGatewayMemory))
}

// Overhead is the sum of the VPN and the gateway requests.
func (c Config) Overhead() Overhead {
	var o Overhead
	for _, r := range []corev1.ResourceRequirements{c.VPN(), c.Gateway()} {
		o.CPU += r.Requests.Cpu().MilliValue()
		o.Memory += r.Requests.Memory().Value()
	}
	return o
}
