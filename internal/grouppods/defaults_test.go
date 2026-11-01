package grouppods

import (
	"testing"

	"github.com/caarlos0/env/v11"
)

// The defaults of the chart as the agent reads them must fit together: the default size of the VPN and the gateway pod (a group made
// with no size of its own) has to be within the MAXIMUM the agent reports, which is the size at groupLimits (not at the sizing's own
// maxUsers/maxLabs). Otherwise the daemon can plan no group at the default size on a default install.
func TestEnvDefaultsFitTheReportedMaximum(t *testing.T) {
	var sz Sizing
	var c Config
	var l Limits
	for _, p := range []any{&sz, &c, &l} {
		if err := env.Parse(p); err != nil {
			t.Fatal(err)
		}
	}
	s, err := sz.Parse()
	if err != nil {
		t.Fatal(err)
	}
	s.VPN.MaxUnits, s.Gateway.MaxUnits = l.MaxUsers, l.MaxLabs
	if err := c.CheckWithin(s); err != nil {
		t.Fatalf("default install: %v", err)
	}
}
