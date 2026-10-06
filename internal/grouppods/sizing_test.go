package grouppods

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func defaultSizing() Sizing {
	return Sizing{
		VPNBaseCPU: "20m", VPNBaseMemory: "64Mi", VPNPerUserCPU: "6m", VPNPerUserMemory: "20Mi", VPNMaxUsers: 20,
		GatewayBaseCPU: "5m", GatewayBaseMemory: "16Mi", GatewayPerLabCPU: "2m", GatewayPerLabMemory: "4Mi", GatewayMaxLabs: 50,
	}
}

func TestSizingMaximum(t *testing.T) {
	s, err := defaultSizing().Parse()
	if err != nil {
		t.Fatal(err)
	}
	if m := s.VPN.Max(); m.CPU != 140 || m.Memory != 464<<20 {
		t.Fatalf("vpn max %+v", m)
	}
	if m := s.Gateway.Max(); m.CPU != 105 || m.Memory != 216<<20 {
		t.Fatalf("gateway max %+v", m)
	}
}

func TestSizingCheck(t *testing.T) {
	s, _ := defaultSizing().Parse()
	for _, tc := range []struct {
		cpu, mem int64
		ok       bool
	}{
		{140, 464 << 20, true}, {1, 1, true}, {141, 64 << 20, false}, {20, 465 << 20, false}, {0, 64 << 20, false}, {20, 0, false}, {-5, 64 << 20, false},
	} {
		if err := s.VPN.Check(tc.cpu, tc.mem); (err == nil) != tc.ok {
			t.Errorf("vpn %dm %d: %v, want ok=%v", tc.cpu, tc.mem, err, tc.ok)
		}
	}
}

func TestSizingParseRefusesNonsense(t *testing.T) {
	for name, mutate := range map[string]func(*Sizing){
		"bad quantity": func(s *Sizing) { s.VPNBaseCPU = "lots" },
		"negative":     func(s *Sizing) { s.GatewayPerLabMemory = "-1Mi" },
		"no users":     func(s *Sizing) { s.VPNMaxUsers = 0 },
		"no labs":      func(s *Sizing) { s.GatewayMaxLabs = -1 },
		"zero maximum": func(s *Sizing) { s.VPNBaseCPU, s.VPNPerUserCPU = "0", "0" },
	} {
		s := defaultSizing()
		mutate(&s)
		if _, err := s.Parse(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The chart's default size of a pod must fit inside the maximum, or the platform could never plan a group at the size the cluster reports.
func TestChartDefaultsMustFitTheMaximum(t *testing.T) {
	s, _ := defaultSizing().Parse()
	if err := (Config{}).CheckWithin(s); err != nil {
		t.Fatalf("the defaults of the chart: %v", err)
	}
	if err := (Config{VPNCPU: "141m"}).CheckWithin(s); err == nil {
		t.Fatal("a default VPN above the maximum was accepted")
	}
	if err := (Config{GatewayMemory: "217Mi"}).CheckWithin(s); err == nil {
		t.Fatal("a default gateway above the maximum was accepted")
	}
}

func TestSizedPodsAreGuaranteed(t *testing.T) {
	c := Config{}
	size := &laboratoryv1alpha1.GroupPodSize{CPUMillicores: 77, MemoryBytes: 99 << 20}
	for name, r := range map[string]corev1.ResourceRequirements{"vpn": c.VPNFor(size), "gateway": c.GatewayFor(size)} {
		for what, l := range map[string]corev1.ResourceList{"requests": r.Requests, "limits": r.Limits} {
			if cpu, mem := l[corev1.ResourceCPU], l[corev1.ResourceMemory]; cpu.MilliValue() != 77 || mem.Value() != 99<<20 {
				t.Errorf("%s %s: %v", name, what, l)
			}
		}
	}
	if got := c.VPNOverhead().CPU; got != 100 {
		t.Errorf("no size: the chart default, got %dm", got)
	}
	if got := c.GatewayOverhead().Memory; got != 32<<20 {
		t.Errorf("no size: the chart default, got %d", got)
	}
}
