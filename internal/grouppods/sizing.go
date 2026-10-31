package grouppods

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Sizing is how the platform sizes the pods of a group (env VPN_BASE_CPU, ..., shared with the chart's values vpn.sizing and
// inetGateway.sizing). The agent reports it in GetFeatures; the backend computes the size of a group's pods from it (the VPN grows
// with the users of the group, the gateway with the labs that use the internet) and passes it to CreateLabGroups; the agent checks
// the size against the maximum here. Measured: the VPN about 16Mi idle, about 195Mi and 55m with 10 active peers; the gateway about
// 1m and 8Mi idle.
type Sizing struct {
	VPNBaseCPU        string `env:"VPN_BASE_CPU" envDefault:"20m"`
	VPNBaseMemory     string `env:"VPN_BASE_MEMORY" envDefault:"64Mi"`
	VPNPerUserCPU     string `env:"VPN_PER_USER_CPU" envDefault:"6m"`
	VPNPerUserMemory  string `env:"VPN_PER_USER_MEMORY" envDefault:"20Mi"`
	VPNMaxUsers       int    `env:"VPN_MAX_USERS" envDefault:"20"`
	GatewayBaseCPU    string `env:"GATEWAY_BASE_CPU" envDefault:"5m"`
	GatewayBaseMemory string `env:"GATEWAY_BASE_MEMORY" envDefault:"16Mi"`
	GatewayPerLabCPU  string `env:"GATEWAY_PER_LAB_CPU" envDefault:"2m"`
	// GatewayPerLabMemory and GatewayMaxLabs: a lab that uses the internet.
	GatewayPerLabMemory string `env:"GATEWAY_PER_LAB_MEMORY" envDefault:"4Mi"`
	GatewayMaxLabs      int    `env:"GATEWAY_MAX_LABS" envDefault:"50"`
}

// PodSizing is one pod's sizing parsed: CPU in millicores, memory in bytes.
type PodSizing struct {
	BaseCPU, BaseMemory       int64
	PerUnitCPU, PerUnitMemory int64
	MaxUnits                  int
}

// Max is the largest size the pod may be given: the base plus the per-unit values at the maximum of units.
func (p PodSizing) Max() Overhead {
	return Overhead{CPU: p.BaseCPU + p.PerUnitCPU*int64(p.MaxUnits), Memory: p.BaseMemory + p.PerUnitMemory*int64(p.MaxUnits)}
}

// Sizings are the VPN and the gateway sizing parsed.
type Sizings struct {
	VPN, Gateway PodSizing
}

// Parse validates the quantities and the counts.
func (s Sizing) Parse() (Sizings, error) {
	var out Sizings
	for _, q := range []struct {
		env, val string
		cpu      bool
		dst      *int64
	}{
		{"VPN_BASE_CPU", s.VPNBaseCPU, true, &out.VPN.BaseCPU},
		{"VPN_BASE_MEMORY", s.VPNBaseMemory, false, &out.VPN.BaseMemory},
		{"VPN_PER_USER_CPU", s.VPNPerUserCPU, true, &out.VPN.PerUnitCPU},
		{"VPN_PER_USER_MEMORY", s.VPNPerUserMemory, false, &out.VPN.PerUnitMemory},
		{"GATEWAY_BASE_CPU", s.GatewayBaseCPU, true, &out.Gateway.BaseCPU},
		{"GATEWAY_BASE_MEMORY", s.GatewayBaseMemory, false, &out.Gateway.BaseMemory},
		{"GATEWAY_PER_LAB_CPU", s.GatewayPerLabCPU, true, &out.Gateway.PerUnitCPU},
		{"GATEWAY_PER_LAB_MEMORY", s.GatewayPerLabMemory, false, &out.Gateway.PerUnitMemory},
	} {
		v, err := resource.ParseQuantity(q.val)
		if err != nil || v.Sign() < 0 {
			return out, fmt.Errorf("%s %q is not a quantity", q.env, q.val)
		}
		if q.cpu {
			*q.dst = v.MilliValue()
		} else {
			*q.dst = v.Value()
		}
	}
	if s.VPNMaxUsers <= 0 || s.GatewayMaxLabs <= 0 {
		return out, fmt.Errorf("VPN_MAX_USERS and GATEWAY_MAX_LABS must be positive")
	}
	out.VPN.MaxUnits, out.Gateway.MaxUnits = s.VPNMaxUsers, s.GatewayMaxLabs
	for name, p := range map[string]PodSizing{"VPN": out.VPN, "gateway": out.Gateway} {
		if p.Max().CPU <= 0 || p.Max().Memory <= 0 {
			return out, fmt.Errorf("the %s sizing must give a positive maximum CPU and memory", name)
		}
	}
	return out, nil
}

// Check says whether a size the backend asked for is one the cluster allows: positive, and at most the maximum of the pod.
func (p PodSizing) Check(cpuMillicores, memoryBytes int64) error {
	if cpuMillicores <= 0 || memoryBytes <= 0 {
		return fmt.Errorf("cpu and memory must both be positive")
	}
	m := p.Max()
	if cpuMillicores > m.CPU {
		return fmt.Errorf("cpu %dm is over the maximum %dm", cpuMillicores, m.CPU)
	}
	if memoryBytes > m.Memory {
		return fmt.Errorf("memory %d bytes is over the maximum %d bytes", memoryBytes, m.Memory)
	}
	return nil
}

// CheckWithin refuses a chart whose default size of a pod is larger than the maximum the sizing allows: the platform could then never
// plan a group at a size the cluster reports as its maximum.
func (c Config) CheckWithin(s Sizings) error {
	if v, m := c.VPNOverhead(), s.VPN.Max(); v.CPU > m.CPU || v.Memory > m.Memory {
		return fmt.Errorf("vpn.resources (%dm, %d bytes) is over the maximum of vpn.sizing (%dm, %d bytes)", v.CPU, v.Memory, m.CPU, m.Memory)
	}
	if g, m := c.GatewayOverhead(), s.Gateway.Max(); g.CPU > m.CPU || g.Memory > m.Memory {
		return fmt.Errorf("inetGateway.resources (%dm, %d bytes) is over the maximum of inetGateway.sizing (%dm, %d bytes)", g.CPU, g.Memory, m.CPU, m.Memory)
	}
	return nil
}
