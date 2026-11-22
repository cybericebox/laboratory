package grpc

import (
	"testing"

	"github.com/cybericebox/laboratory/internal/switches"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// GetFeatures reports the console cables (contract 3.1): the chart switch, the limits, and the port constants of a device.
func TestFeaturesReportConsoleCables(t *testing.T) {
	for _, on := range []bool{false, true} {
		f := testFeatures
		sw := switches.Everything()
		sw.ConsoleCables = on
		f.Switches = &sw
		h := featuresHandler(t, f, newTenantTenant("a", true, nil))
		got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		cc := got.GetConsoleCables()
		if cc == nil || cc.GetEnabled() != on || cc.GetMaxCablesPerLab() != 64 || cc.GetMaxBytesPerSecond() != 65536 {
			t.Fatalf("switch %v: console cables %+v", on, cc)
		}
		if c := got.GetConstants(); c.GetMaxConsolePorts() != 4 || c.GetMaxSerialPorts() != 16 {
			t.Fatalf("constants: %+v", c)
		}
	}
}
