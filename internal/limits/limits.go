// Package limits holds the caps the cluster owns on devices, labs and tenants. The chart sets them (values
// limits.*); the management agent enforces them on CreateLabs and reports them in GetFeatures, and the planning
// profile (the default of a device without resources) is the one the scheduler counts with.
package limits

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// Config is read by the agent from the environment the chart gives it. CPU is a Kubernetes quantity ("500m", "2"),
// memory too ("512Mi"). The defaults mirror values.yaml.
type Config struct {
	DeviceMaxCPU        string `env:"AGENT_LIMIT_DEVICE_MAX_CPU" envDefault:"2000m"`
	DeviceMaxMemory     string `env:"AGENT_LIMIT_DEVICE_MAX_MEMORY" envDefault:"4Gi"`
	DeviceDefaultCPU    string `env:"AGENT_LIMIT_DEVICE_DEFAULT_CPU" envDefault:"100m"`
	DeviceDefaultMemory string `env:"AGENT_LIMIT_DEVICE_DEFAULT_MEMORY" envDefault:"256Mi"`
	// LabMaxDevices caps the devices of one lab (0 = no limit); LabMaxCPU and LabMaxMemory cap the sum of the
	// resources of its devices (the planning profile counts for a device without resources).
	LabMaxDevices int    `env:"AGENT_LIMIT_LAB_MAX_DEVICES" envDefault:"20"`
	LabMaxCPU     string `env:"AGENT_LIMIT_LAB_MAX_CPU" envDefault:"4000m"`
	LabMaxMemory  string `env:"AGENT_LIMIT_LAB_MAX_MEMORY" envDefault:"8Gi"`
	// TenantMaxLabs caps the labs of one tenant (0 = no limit; the tenant's resource quota still applies).
	TenantMaxLabs int `env:"AGENT_LIMIT_TENANT_MAX_LABS" envDefault:"0"`
}

// Limits is Config parsed: CPU in millicores, memory in bytes. A zero cap means no limit.
type Limits struct {
	DeviceMaxCPU, DeviceMaxMemory         int64
	DeviceDefaultCPU, DeviceDefaultMemory int64
	LabMaxDevices                         int
	LabMaxCPU, LabMaxMemory               int64
	TenantMaxLabs                         int
}

// Parse validates the quantities and the counts.
func (c Config) Parse() (Limits, error) {
	var l Limits
	for _, q := range []struct {
		env, val string
		cpu      bool
		dst      *int64
	}{
		{"AGENT_LIMIT_DEVICE_MAX_CPU", c.DeviceMaxCPU, true, &l.DeviceMaxCPU},
		{"AGENT_LIMIT_DEVICE_MAX_MEMORY", c.DeviceMaxMemory, false, &l.DeviceMaxMemory},
		{"AGENT_LIMIT_DEVICE_DEFAULT_CPU", c.DeviceDefaultCPU, true, &l.DeviceDefaultCPU},
		{"AGENT_LIMIT_DEVICE_DEFAULT_MEMORY", c.DeviceDefaultMemory, false, &l.DeviceDefaultMemory},
		{"AGENT_LIMIT_LAB_MAX_CPU", c.LabMaxCPU, true, &l.LabMaxCPU},
		{"AGENT_LIMIT_LAB_MAX_MEMORY", c.LabMaxMemory, false, &l.LabMaxMemory},
	} {
		v, err := quantity(q.val, q.cpu)
		if err != nil {
			return l, fmt.Errorf("%s %q: %w", q.env, q.val, err)
		}
		*q.dst = v
	}
	if c.LabMaxDevices < 0 || c.TenantMaxLabs < 0 {
		return l, fmt.Errorf("AGENT_LIMIT_LAB_MAX_DEVICES and AGENT_LIMIT_TENANT_MAX_LABS must not be negative")
	}
	l.LabMaxDevices, l.TenantMaxLabs = c.LabMaxDevices, c.TenantMaxLabs
	if l.DeviceMaxCPU > 0 && l.DeviceDefaultCPU > l.DeviceMaxCPU || l.DeviceMaxMemory > 0 && l.DeviceDefaultMemory > l.DeviceMaxMemory {
		return l, fmt.Errorf("the default resources of a device must not exceed its maximum")
	}
	return l, nil
}

func quantity(s string, cpu bool) (int64, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, err
	}
	if q.Sign() < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	if cpu {
		return q.MilliValue(), nil
	}
	return q.Value(), nil
}

// DeviceResources is what a device is planned with: its limit, else its request, else the default. A quantity
// that does not parse is an error.
func (l Limits) DeviceResources(r *laboratoryv1alpha1.DeviceResources) (cpu, mem int64, err error) {
	cpu, mem = l.DeviceDefaultCPU, l.DeviceDefaultMemory
	if r == nil {
		return cpu, mem, nil
	}
	pick := func(name string, cpuRes bool, candidates ...string) (int64, bool, error) {
		for _, c := range candidates {
			if c == "" {
				continue
			}
			v, err := quantity(c, cpuRes)
			if err != nil {
				return 0, false, fmt.Errorf("%s %q: %w", name, c, err)
			}
			return v, true, nil
		}
		return 0, false, nil
	}
	if v, ok, err := pick("cpuLimit/cpuRequest", true, r.CPULimit, r.CPURequest); err != nil {
		return 0, 0, err
	} else if ok {
		cpu = v
	}
	if v, ok, err := pick("memoryLimit/memoryRequest", false, r.MemoryLimit, r.MemoryRequest); err != nil {
		return 0, 0, err
	} else if ok {
		mem = v
	}
	return cpu, mem, nil
}

// CheckSpec refuses a lab spec that passes a cap: too many container devices (switches and hubs run no pod and
// do not count), a device over the device maximum, or devices
// that together pass the lab's CPU or memory cap. The message names the device and both numbers.
func (l Limits) CheckSpec(spec *laboratoryv1alpha1.LabSpec) error {
	containers := 0
	for i := range spec.Devices {
		if spec.Devices[i].Type == laboratoryv1alpha1.DeviceTypeContainer {
			containers++
		}
	}
	if l.LabMaxDevices > 0 && containers > l.LabMaxDevices {
		return fmt.Errorf("the lab has %d container devices, the limit is %d", containers, l.LabMaxDevices)
	}
	var sumCPU, sumMem int64
	for i := range spec.Devices {
		d := &spec.Devices[i]
		if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
			continue // a switch or a hub runs no pod of its own
		}
		cpu, mem, err := l.DeviceResources(d.Resources)
		if err != nil {
			return fmt.Errorf("device %q: %w", d.Name, err)
		}
		if l.DeviceMaxCPU > 0 && cpu > l.DeviceMaxCPU {
			return fmt.Errorf("device %q: cpu %dm exceeds the limit of %dm per device", d.Name, cpu, l.DeviceMaxCPU)
		}
		if l.DeviceMaxMemory > 0 && mem > l.DeviceMaxMemory {
			return fmt.Errorf("device %q: memory %d bytes exceeds the limit of %d bytes per device", d.Name, mem, l.DeviceMaxMemory)
		}
		sumCPU, sumMem = sumCPU+cpu, sumMem+mem
	}
	if l.LabMaxCPU > 0 && sumCPU > l.LabMaxCPU {
		return fmt.Errorf("the devices of the lab need %dm of cpu, the limit per lab is %dm", sumCPU, l.LabMaxCPU)
	}
	if l.LabMaxMemory > 0 && sumMem > l.LabMaxMemory {
		return fmt.Errorf("the devices of the lab need %d bytes of memory, the limit per lab is %d bytes", sumMem, l.LabMaxMemory)
	}
	return nil
}
