package grouppods

import (
	"testing"

	"github.com/caarlos0/env/v11"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func TestDefaultsAreGuaranteedAndSummed(t *testing.T) {
	c := Config{VPNCPU: "100m", VPNMemory: "64Mi", GatewayCPU: "50m", GatewayMemory: "32Mi"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	vpn := c.VPN()
	if !vpn.Requests.Cpu().Equal(*vpn.Limits.Cpu()) || !vpn.Requests.Memory().Equal(*vpn.Limits.Memory()) {
		t.Fatal("requests = limits")
	}
	if o := c.Overhead(); o.CPU != 150 || o.Memory != 96<<20 {
		t.Fatalf("overhead %+v", o)
	}
}

func TestValidateRefusesBadValues(t *testing.T) {
	good := Config{VPNCPU: "100m", VPNMemory: "64Mi", GatewayCPU: "50m", GatewayMemory: "32Mi"}
	for name, mutate := range map[string]func(*Config){
		"junk":     func(c *Config) { c.VPNCPU = "lots" },
		"zero":     func(c *Config) { c.GatewayMemory = "0" },
		"negative": func(c *Config) { c.VPNMemory = "-1Mi" },
		"empty":    func(c *Config) { c.GatewayCPU = "" },
	} {
		c := good
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestEmptyConfigFallsBackToTheDefaults(t *testing.T) {
	if o := (Config{}).Overhead(); o.CPU != 110 || o.Memory != 352<<20 {
		t.Fatalf("%+v", o)
	}
}

func TestDefaultsAreTheMeasuredOnes(t *testing.T) {
	var c Config
	if err := env.Parse(&c); err != nil {
		t.Fatal(err)
	}
	if c.VPNCPU != "80m" || c.VPNMemory != "264Mi" || c.GatewayCPU != "10m" || c.GatewayMemory != "32Mi" {
		t.Fatalf("defaults %+v", c)
	}
}

func TestGatewayPodFollowsTheLabsAndTheVPNFollowsTheSwitch(t *testing.T) {
	internet := laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{{Name: "out", Kind: laboratoryv1alpha1.GatewayKindInternet}}}}
	vpn := laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{{Name: "in", Kind: laboratoryv1alpha1.GatewayKindVPN}}}}
	plain := laboratoryv1alpha1.Lab{}
	for name, tc := range map[string]struct {
		labs []laboratoryv1alpha1.Lab
		want bool
	}{
		"no labs":            {nil, false},
		"a lab without any":  {[]laboratoryv1alpha1.Lab{plain}, false},
		"only a vpn gateway": {[]laboratoryv1alpha1.Lab{vpn}, false},
		"an internet one":    {[]laboratoryv1alpha1.Lab{plain, vpn, internet}, true},
	} {
		if got := GatewayNeeded(tc.labs); got != tc.want {
			t.Errorf("%s: GatewayNeeded = %v, want %v", name, got, tc.want)
		}
	}

	f, tr := false, true
	for name, tc := range map[string]struct {
		lg   laboratoryv1alpha1.LabGroup
		want []string
	}{
		"not counted yet keeps both": {laboratoryv1alpha1.LabGroup{}, []string{names.ComponentVPN, names.ComponentGateway}},
		"counted: no gateway":        {laboratoryv1alpha1.LabGroup{Status: laboratoryv1alpha1.LabGroupStatus{GatewayPod: &f}}, []string{names.ComponentVPN}},
		"counted: gateway":           {laboratoryv1alpha1.LabGroup{Status: laboratoryv1alpha1.LabGroupStatus{GatewayPod: &tr}}, []string{names.ComponentVPN, names.ComponentGateway}},
		"vpn disabled, gateway":      {laboratoryv1alpha1.LabGroup{Spec: laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}}, Status: laboratoryv1alpha1.LabGroupStatus{GatewayPod: &tr}}, []string{names.ComponentGateway}},
		"vpn disabled, no gateway":   {laboratoryv1alpha1.LabGroup{Spec: laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}}, Status: laboratoryv1alpha1.LabGroupStatus{GatewayPod: &f}}, nil},
	} {
		got := PodNames(&tc.lg)
		if len(got) != len(tc.want) {
			t.Errorf("%s: PodNames = %v, want %v", name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: PodNames = %v, want %v", name, got, tc.want)
			}
		}
	}
	// the need of the group follows the pods it runs
	g := laboratoryv1alpha1.LabGroup{Status: laboratoryv1alpha1.LabGroupStatus{GatewayPod: &f}}
	c := Config{VPNCPU: "100m", VPNMemory: "64Mi", GatewayCPU: "50m", GatewayMemory: "32Mi"}
	if o := c.Need(&g); o.CPU != 100 || o.Memory != 64<<20 {
		t.Errorf("a group without a gateway pod requests only the VPN pod: %+v", o)
	}
}
