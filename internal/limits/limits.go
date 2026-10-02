// Package limits holds the caps the cluster owns on devices, labs and tenants. The chart sets them (values
// limits.*); the management agent enforces them on CreateLabs and reports them in GetFeatures, and the planning
// profile (the default of a device without resources) is the one the scheduler counts with.
package limits

import (
	"fmt"
	"math"

	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// Config is read by the agent from the environment the chart gives it. CPU is a Kubernetes quantity ("500m", "2"),
// memory too ("512Mi"). The defaults mirror values.yaml.
type Config struct {
	DeviceMaxCPU        string `env:"AGENT_LIMIT_DEVICE_MAX_CPU" envDefault:"2000m"`
	DeviceMaxMemory     string `env:"AGENT_LIMIT_DEVICE_MAX_MEMORY" envDefault:"4Gi"`
	DeviceDefaultCPU    string `env:"AGENT_LIMIT_DEVICE_DEFAULT_CPU" envDefault:"100m"`
	DeviceDefaultMemory string `env:"AGENT_LIMIT_DEVICE_DEFAULT_MEMORY" envDefault:"256Mi"`
	// LabMaxDevices caps the devices of one lab, switches and hubs included (0 = the platform ceiling, names.MaxLabDevices).
	LabMaxDevices int `env:"AGENT_LIMIT_LAB_MAX_DEVICES" envDefault:"20"`
	// GroupMaxLabs caps the labs of one LabGroup (0 = no limit); GroupMaxCPU and GroupMaxMemory cap the sum of
	// the resources of the devices of all its labs (the planning profile counts for a device without resources;
	// "0" = no limit).
	GroupMaxLabs   int    `env:"AGENT_LIMIT_GROUP_MAX_LABS" envDefault:"50"`
	GroupMaxCPU    string `env:"AGENT_LIMIT_GROUP_MAX_CPU" envDefault:"0"`
	GroupMaxMemory string `env:"AGENT_LIMIT_GROUP_MAX_MEMORY" envDefault:"0"`
	TenantMaxLabs  int    `env:"AGENT_LIMIT_TENANT_MAX_LABS" envDefault:"0"`
}

// Limits is Config parsed: CPU in millicores, memory in bytes. A zero cap means no limit.
type Limits struct {
	DeviceMaxCPU, DeviceMaxMemory         int64
	DeviceDefaultCPU, DeviceDefaultMemory int64
	LabMaxDevices                         int
	GroupMaxLabs                          int
	GroupMaxCPU, GroupMaxMemory           int64
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
		{"AGENT_LIMIT_GROUP_MAX_CPU", c.GroupMaxCPU, true, &l.GroupMaxCPU},
		{"AGENT_LIMIT_GROUP_MAX_MEMORY", c.GroupMaxMemory, false, &l.GroupMaxMemory},
	} {
		v, err := quantity(q.val, q.cpu)
		if err != nil {
			return l, fmt.Errorf("%s %q: %w", q.env, q.val, err)
		}
		*q.dst = v
	}
	if c.LabMaxDevices < 0 || c.GroupMaxLabs < 0 || c.TenantMaxLabs < 0 {
		return l, fmt.Errorf("AGENT_LIMIT_LAB_MAX_DEVICES, AGENT_LIMIT_GROUP_MAX_LABS and AGENT_LIMIT_TENANT_MAX_LABS must not be negative")
	}
	l.LabMaxDevices, l.GroupMaxLabs, l.TenantMaxLabs = c.LabMaxDevices, c.GroupMaxLabs, c.TenantMaxLabs
	// The chart can lower the ceiling of the devices of a lab, never lift it (0 = the ceiling itself).
	if l.LabMaxDevices == 0 || l.LabMaxDevices > names.MaxLabDevices {
		l.LabMaxDevices = names.MaxLabDevices
	}
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

// The absolute bounds of one device quantity, whatever the chart maxima say: a value above them is a mistake or an
// attempt to overflow the sums (1024 cores, 1 TiB).
var (
	absoluteCPU    = resource.MustParse("1024")
	absoluteMemory = resource.MustParse("1Ti")
)

// DeviceQuantity parses a device cpu (millicores) or memory (bytes) value the way a tenant may write it: it must be
// positive and within the absolute bound. A zero value would remove the pod limit, a negative or an overflowing one
// would skew the sums. Quantities are compared as quantities, so an exponent that overflows int64 is refused, not wrapped.
func DeviceQuantity(s string, cpu bool) (int64, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, err
	}
	if q.Sign() <= 0 {
		return 0, fmt.Errorf("must be greater than zero")
	}
	bound := absoluteMemory
	if cpu {
		bound = absoluteCPU
	}
	if q.Cmp(bound) > 0 {
		return 0, fmt.Errorf("must not exceed %s", bound.String())
	}
	if cpu {
		return q.MilliValue(), nil
	}
	return q.Value(), nil
}

// addSat adds without wrapping around.
func addSat(a, b int64) int64 {
	if a > 0 && b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
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
			v, err := DeviceQuantity(c, cpuRes)
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

// SpecTotals is the CPU (millicores) and memory (bytes) the container devices of a spec are planned with, and
// how many there are. A quantity that does not parse is an error naming the device.
func (l Limits) SpecTotals(spec *laboratoryv1alpha1.LabSpec) (cpu, mem int64, containers int, err error) {
	for i := range spec.Devices {
		d := &spec.Devices[i]
		if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
			continue // a switch or a hub runs no pod of its own
		}
		c, m, err := l.DeviceResources(d.Resources)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("device %q: %w", d.Name, err)
		}
		cpu, mem, containers = addSat(cpu, c), addSat(mem, m), containers+1
	}
	return cpu, mem, containers, nil
}

// CheckSpec refuses a lab spec that passes a cap: too many devices (every kind counts: a switch or a hub takes a VNI from the shared
// pool, so it is no cheaper than a container), too many interfaces on a device, too many connections, or a device over the device
// maximum. The device, interface and connection ceilings are constants of the platform (names.MaxLabDevices and friends) that the
// chart can only lower; the message names the numbers.
func (l Limits) CheckSpec(spec *laboratoryv1alpha1.LabSpec) error {
	if _, _, _, err := l.SpecTotals(spec); err != nil {
		return err
	}
	maxDevices := names.MaxLabDevices
	if l.LabMaxDevices > 0 && l.LabMaxDevices < maxDevices {
		maxDevices = l.LabMaxDevices
	}
	if n := len(spec.Devices); n > maxDevices {
		return fmt.Errorf("the lab has %d devices (switches and hubs included), the limit is %d", n, maxDevices)
	}
	if n := len(spec.Connections); n > names.MaxLabConnections {
		return fmt.Errorf("the lab has %d connections, the limit is %d", n, names.MaxLabConnections)
	}
	for i := range spec.Devices {
		if n := len(spec.Devices[i].Interfaces); n > names.MaxDeviceInterfaces {
			return fmt.Errorf("device %q has %d interfaces, the limit is %d", spec.Devices[i].Name, n, names.MaxDeviceInterfaces)
		}
	}
	for i := range spec.Devices {
		d := &spec.Devices[i]
		if d.Type != laboratoryv1alpha1.DeviceTypeContainer {
			continue
		}
		cpu, mem, _ := l.DeviceResources(d.Resources)
		if l.DeviceMaxCPU > 0 && cpu > l.DeviceMaxCPU {
			return fmt.Errorf("device %q: cpu %dm exceeds the limit of %dm per device", d.Name, cpu, l.DeviceMaxCPU)
		}
		if l.DeviceMaxMemory > 0 && mem > l.DeviceMaxMemory {
			return fmt.Errorf("device %q: memory %d bytes exceeds the limit of %d bytes per device", d.Name, mem, l.DeviceMaxMemory)
		}
	}
	return nil
}

// GroupFits says whether a group that already holds labs labs planned at cpu and mem can take one more lab
// of the given totals; the error says which cap stops it.
func (l Limits) GroupFits(labs int, cpu, mem, addCPU, addMem int64) error {
	switch {
	case l.GroupMaxLabs > 0 && labs+1 > l.GroupMaxLabs:
		return fmt.Errorf("the lab group is at its limit of %d labs", l.GroupMaxLabs)
	case l.GroupMaxCPU > 0 && addSat(cpu, addCPU) > l.GroupMaxCPU:
		return fmt.Errorf("the lab needs %dm of cpu and the lab group already plans %dm, the limit per group is %dm", addCPU, cpu, l.GroupMaxCPU)
	case l.GroupMaxMemory > 0 && addSat(mem, addMem) > l.GroupMaxMemory:
		return fmt.Errorf("the lab needs %d bytes of memory and the lab group already plans %d, the limit per group is %d bytes", addMem, mem, l.GroupMaxMemory)
	}
	return nil
}
