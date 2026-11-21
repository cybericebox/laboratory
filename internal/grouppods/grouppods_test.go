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

// The pods a group runs follow its two switches; a group made before them keeps both unless its VPN is disabled; Enabled wins over Disabled.
func TestPodNamesFollowTheSwitches(t *testing.T) {
	on, off := true, false
	for name, tc := range map[string]struct {
		spec laboratoryv1alpha1.LabGroupSpec
		want []string
	}{
		"nothing set (a group made before the switches)": {laboratoryv1alpha1.LabGroupSpec{}, []string{names.ComponentVPN, names.ComponentGateway}},
		"old disabled":               {laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}}, []string{names.ComponentGateway}},
		"enabled wins over disabled": {laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true, Enabled: &on}}, []string{names.ComponentVPN, names.ComponentGateway}},
		"both off":                   {laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Enabled: &off}, Gateway: laboratoryv1alpha1.LabGroupGatewaySpec{Enabled: &off}}, nil},
		"only the gateway":           {laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Enabled: &off}, Gateway: laboratoryv1alpha1.LabGroupGatewaySpec{Enabled: &on}}, []string{names.ComponentGateway}},
		"only the VPN":               {laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Enabled: &on}, Gateway: laboratoryv1alpha1.LabGroupGatewaySpec{Enabled: &off}}, []string{names.ComponentVPN}},
	} {
		got := PodNames(&laboratoryv1alpha1.LabGroup{Spec: tc.spec})
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
	// what a group requests follows the pods it runs
	c := Config{VPNCPU: "100m", VPNMemory: "64Mi", GatewayCPU: "50m", GatewayMemory: "32Mi"}
	vpnOnly := laboratoryv1alpha1.LabGroup{Spec: laboratoryv1alpha1.LabGroupSpec{Gateway: laboratoryv1alpha1.LabGroupGatewaySpec{Enabled: &off}}}
	if o := c.Need(&vpnOnly); o.CPU != 100 || o.Memory != 64<<20 {
		t.Errorf("a group with no gateway pod requests only the VPN pod: %+v", o)
	}
	if o := c.Need(&laboratoryv1alpha1.LabGroup{Spec: laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Enabled: &off}, Gateway: laboratoryv1alpha1.LabGroupGatewaySpec{Enabled: &off}}}); o.CPU != 0 || o.Memory != 0 {
		t.Errorf("a group with both servers off requests nothing: %+v", o)
	}
}
