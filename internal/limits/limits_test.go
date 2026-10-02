package limits

import (
	"strings"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func dev(name string, r *laboratoryv1alpha1.DeviceResources) laboratoryv1alpha1.DeviceTemplate {
	return laboratoryv1alpha1.DeviceTemplate{Name: name, Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: r}
}

func chartDefaults(t *testing.T) Limits {
	t.Helper()
	l, err := Config{
		DeviceMaxCPU: "500m", DeviceMaxMemory: "512Mi", DeviceDefaultCPU: "100m", DeviceDefaultMemory: "256Mi",
		LabMaxDevices: 10, LabMaxCPU: "2", LabMaxMemory: "2Gi",
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
		{"ten devices of the profile (1 CPU, 2.5Gi)", many(10, nil), "memory"},
		{"eleven devices", many(11, nil), "11 container devices"},
		{"device over cpu", many(1, &laboratoryv1alpha1.DeviceResources{CPULimit: "1"}), `device "a": cpu 1000m exceeds the limit of 500m`},
		{"device over memory", many(1, &laboratoryv1alpha1.DeviceResources{MemoryRequest: "1Gi"}), "memory"},
		{"declared at the maximum", many(2, &laboratoryv1alpha1.DeviceResources{CPULimit: "500m", MemoryLimit: "512Mi"}), ""},
		{"sum over the lab cpu", many(5, &laboratoryv1alpha1.DeviceResources{CPULimit: "500m", MemoryLimit: "64Mi"}), "need 2500m of cpu"},
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
	// switches and hubs run no pod
	sw := &laboratoryv1alpha1.LabSpec{}
	for i := 0; i < 20; i++ {
		sw.Devices = append(sw.Devices, laboratoryv1alpha1.DeviceTemplate{Name: "s", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch})
	}
	if err := l.CheckSpec(sw); err != nil {
		t.Errorf("switches do not count: %v", err)
	}
	// 0 = no limit
	if err := (Limits{}).CheckSpec(many(30, &laboratoryv1alpha1.DeviceResources{CPULimit: "64"})); err != nil {
		t.Errorf("no limits: %v", err)
	}
}

func TestParseRefusesBadValues(t *testing.T) {
	ok := Config{DeviceMaxCPU: "500m", DeviceMaxMemory: "512Mi", DeviceDefaultCPU: "100m", DeviceDefaultMemory: "256Mi", LabMaxCPU: "2", LabMaxMemory: "2Gi"}
	if _, err := ok.Parse(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"bad cpu":          func(c *Config) { c.DeviceMaxCPU = "x" },
		"negative devices": func(c *Config) { c.LabMaxDevices = -1 },
		"negative labs":    func(c *Config) { c.TenantMaxLabs = -1 },
		"default over max": func(c *Config) { c.DeviceDefaultMemory = "1Gi" },
	} {
		c := ok
		mutate(&c)
		if _, err := c.Parse(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}
