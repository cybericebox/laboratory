package grouppods

import "testing"

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
	if o := (Config{}).Overhead(); o.CPU != 150 || o.Memory != 96<<20 {
		t.Fatalf("%+v", o)
	}
}
