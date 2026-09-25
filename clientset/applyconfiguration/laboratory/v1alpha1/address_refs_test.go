package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestAddressReferenceApplyConfigurationJSON(t *testing.T) {
	config := AddrSpec().WithType(laboratoryv1alpha1.AddrTypeStatic).
		WithAddressRef(NetworkIPRef().WithNetwork("vpn").WithHost(10)).
		WithGatewayRef(NetworkIPRef().WithNetwork("vpn").WithHost(1)).
		WithRoutes(Route().WithDstRef(NetworkSubnetRef().WithNetwork("internet")).
			WithViaRef(NetworkIPRef().WithNetwork("vpn").WithHost(1)))
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"addressRef":{"network":"vpn","host":10}`, `"gatewayRef":{"network":"vpn","host":1}`, `"dstRef":{"network":"internet"}`, `"viaRef":{"network":"vpn","host":1}`} {
		if !strings.Contains(string(body), field) {
			t.Fatalf("missing %s in %s", field, body)
		}
	}
}
