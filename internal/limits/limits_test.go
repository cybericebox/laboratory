package limits

import (
	"math"
	"strings"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func dev(name string, r *laboratoryv1alpha1.DeviceResources) laboratoryv1alpha1.DeviceTemplate {
	return laboratoryv1alpha1.DeviceTemplate{Name: name, Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: r}
}

func chartDefaults(t *testing.T) Limits {
	t.Helper()
	l, err := Config{
		DeviceMaxCPU: "500m", DeviceMaxMemory: "512Mi", DeviceDefaultCPU: "100m", DeviceDefaultMemory: "256Mi",
		LabMaxDevices: 10, GroupMaxLabs: 50, GroupMaxCPU: "0", GroupMaxMemory: "0",
	}.Parse()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestCheckSpec(t *testing.T) {
	l := chartDefaults(t)
	many := func(n int, r *laboratoryv1alpha1.DeviceResources) *laboratoryv1alpha1.LabSpec {
		s := &laboratoryv1alpha1.LabSpec{}
		for i := 0; i < n; i++ {
			s.Devices = append(s.Devices, dev(string(rune('a'+i)), r))
		}
		return s
	}
	cases := []struct {
		name string
		spec *laboratoryv1alpha1.LabSpec
		want string // empty: allowed
	}{
		{"no resources: the profile counts", many(5, nil), ""},
		{"ten devices of the profile", many(10, nil), ""},
		{"eleven devices", many(11, nil), "11 devices"},
		{"device over cpu", many(1, &laboratoryv1alpha1.DeviceResources{CPULimit: "1"}), `device "a": cpu 1000m exceeds the limit of 500m`},
		{"device over memory", many(1, &laboratoryv1alpha1.DeviceResources{MemoryRequest: "1Gi"}), "memory"},
		{"declared at the maximum", many(2, &laboratoryv1alpha1.DeviceResources{CPULimit: "500m", MemoryLimit: "512Mi"}), ""},
		{"bad quantity", many(1, &laboratoryv1alpha1.DeviceResources{CPULimit: "lots"}), `device "a"`},
	}
	for _, tc := range cases {
		err := l.CheckSpec(tc.spec)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// switches and hubs count toward the device limit: each takes a VNI from the shared pool
	sw := &laboratoryv1alpha1.LabSpec{}
	for i := 0; i < 20; i++ {
		sw.Devices = append(sw.Devices, laboratoryv1alpha1.DeviceTemplate{Name: "s", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch})
	}
	if err := l.CheckSpec(sw); err == nil || !strings.Contains(err.Error(), "switches and hubs included") {
		t.Errorf("switches count: %v", err)
	}
	mixed := many(8, nil)
	for i := 0; i < 3; i++ {
		mixed.Devices = append(mixed.Devices, laboratoryv1alpha1.DeviceTemplate{Name: "s", Type: laboratoryv1alpha1.DeviceTypeHub})
	}
	if err := l.CheckSpec(mixed); err == nil {
		t.Error("8 containers and 3 hubs are 11 devices")
	}
	// 0 = the platform ceiling, never more
	if err := (Limits{}).CheckSpec(many(30, &laboratoryv1alpha1.DeviceResources{CPULimit: "64"})); err != nil {
		t.Errorf("no configured limit: %v", err)
	}
	if err := (Limits{}).CheckSpec(many(names.MaxLabDevices+1, nil)); err == nil {
		t.Error("the ceiling is a constant of the platform")
	}
}

// 16 interfaces on a container device, 48 on a switch or hub; the chart can only lower the device ceiling.
func TestHardcodedLabCeilings(t *testing.T) {
	l := chartDefaults(t)
	mk := func(typ laboratoryv1alpha1.DeviceType, n int) *laboratoryv1alpha1.LabSpec {
		d := dev("a", nil)
		d.Type = typ
		for i := 0; i < n; i++ {
			d.Interfaces = append(d.Interfaces, laboratoryv1alpha1.InterfaceSpec{Name: "eth1"})
		}
		return &laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{d}}
	}
	if err := l.CheckSpec(mk(laboratoryv1alpha1.DeviceTypeContainer, names.MaxContainerInterfaces)); err != nil {
		t.Errorf("16 on a container: %v", err)
	}
	if err := l.CheckSpec(mk(laboratoryv1alpha1.DeviceTypeContainer, names.MaxContainerInterfaces+1)); err == nil || !strings.Contains(err.Error(), "interfaces") {
		t.Errorf("17 on a container: %v", err)
	}
	if err := l.CheckSpec(mk(laboratoryv1alpha1.DeviceTypeUnmanagedSwitch, names.MaxSwitchPorts)); err != nil {
		t.Errorf("48 on a switch: %v", err)
	}
	if err := l.CheckSpec(mk(laboratoryv1alpha1.DeviceTypeUnmanagedSwitch, names.MaxSwitchPorts+1)); err == nil {
		t.Error("49 on a switch")
	}
	c, err := Config{DeviceMaxCPU: "1", DeviceMaxMemory: "1Gi", DeviceDefaultCPU: "100m", DeviceDefaultMemory: "1Mi", LabMaxDevices: 1000, GroupMaxCPU: "0", GroupMaxMemory: "0"}.Parse()
	if err != nil || c.LabMaxDevices != names.MaxLabDevices {
		t.Errorf("the chart cannot lift the ceiling: %d %v", c.LabMaxDevices, err)
	}
	c, _ = Config{DeviceMaxCPU: "1", DeviceMaxMemory: "1Gi", DeviceDefaultCPU: "100m", DeviceDefaultMemory: "1Mi", GroupMaxCPU: "0", GroupMaxMemory: "0"}.Parse()
	if c.LabMaxDevices != names.MaxLabDevices {
		t.Errorf("0 means the ceiling: %d", c.LabMaxDevices)
	}
}

func TestParseRefusesBadValues(t *testing.T) {
	ok := Config{DeviceMaxCPU: "500m", DeviceMaxMemory: "512Mi", DeviceDefaultCPU: "100m", DeviceDefaultMemory: "256Mi", GroupMaxCPU: "0", GroupMaxMemory: "0"}
	if _, err := ok.Parse(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"bad cpu":          func(c *Config) { c.DeviceMaxCPU = "x" },
		"negative devices": func(c *Config) { c.LabMaxDevices = -1 },
		"negative labs":    func(c *Config) { c.TenantMaxLabs = -1 },
		"negative group":   func(c *Config) { c.GroupMaxLabs = -1 },
		"bad group cpu":    func(c *Config) { c.GroupMaxCPU = "x" },
		"default over max": func(c *Config) { c.DeviceDefaultMemory = "1Gi" },
	} {
		c := ok
		mutate(&c)
		if _, err := c.Parse(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestGroupFits(t *testing.T) {
	l := Limits{GroupMaxLabs: 3, GroupMaxCPU: 1000, GroupMaxMemory: 1 << 30}
	if err := l.GroupFits(2, 600, 1<<29, 400, 1<<29); err != nil {
		t.Fatalf("exactly at the caps: %v", err)
	}
	for name, err := range map[string]error{
		"labs":   l.GroupFits(3, 0, 0, 1, 1),
		"cpu":    l.GroupFits(1, 600, 0, 401, 0),
		"memory": l.GroupFits(1, 0, 1<<29, 0, 1<<29+1),
	} {
		if err == nil {
			t.Errorf("%s over the cap must be refused", name)
		}
	}
	if err := (Limits{}).GroupFits(1000, 1<<40, 1<<50, 1<<40, 1<<50); err != nil {
		t.Fatalf("0 = no limit: %v", err)
	}
}

func TestSpecTotalsCountContainersAtTheProfile(t *testing.T) {
	l := Limits{DeviceDefaultCPU: 100, DeviceDefaultMemory: 256 << 20}
	spec := &laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		dev("a", nil), dev("b", &laboratoryv1alpha1.DeviceResources{CPURequest: "300m", CPULimit: "400m", MemoryRequest: "64Mi"}),
		{Name: "sw", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
	}}
	cpu, mem, n, err := l.SpecTotals(spec)
	if err != nil || n != 2 || cpu != 500 || mem != (256+64)<<20 {
		t.Fatalf("cpu %d mem %d n %d err %v", cpu, mem, n, err)
	}
}

func TestDeviceQuantityRefusesZeroNegativeAndOverflow(t *testing.T) {
	for _, c := range []struct {
		in  string
		cpu bool
		ok  bool
	}{
		{"0", true, false}, {"0", false, false}, {"0m", true, false}, {"-1", true, false}, {"-1Gi", false, false},
		{"18446744073709551", true, false}, {"1e30", true, false}, {"1e30", false, false}, {"9223372036854775807", false, false},
		{"2Ti", false, false}, {"2000", true, false},
		{"100m", true, true}, {"2", true, true}, {"256Mi", false, true}, {"1Ti", false, true}, {"1024", true, true},
	} {
		_, err := DeviceQuantity(c.in, c.cpu)
		if (err == nil) != c.ok {
			t.Errorf("DeviceQuantity(%q, cpu=%v) = %v, want ok=%v", c.in, c.cpu, err, c.ok)
		}
	}
}

func TestCheckSpecRefusesZeroLimit(t *testing.T) {
	l := chartDefaults(t)
	spec := &laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		dev("a", &laboratoryv1alpha1.DeviceResources{MemoryLimit: "0", CPULimit: "0"})}}
	if err := l.CheckSpec(spec); err == nil {
		t.Fatal("a zero limit must be refused")
	}
}

func TestAddSatDoesNotWrap(t *testing.T) {
	if got := addSat(math.MaxInt64-1, 10); got != math.MaxInt64 {
		t.Errorf("addSat = %d", got)
	}
}
