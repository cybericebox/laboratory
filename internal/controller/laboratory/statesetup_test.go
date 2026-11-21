package laboratory

import (
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/operator"
)

// The operator's token never travels in the clear: a registry without a certificate authority is refused unless it is the development setup,
// which sends no token.
func TestSetupStateRefusesATokenWithoutTLS(t *testing.T) {
	c := operator.StateConfig{Enabled: true, RegistryAddr: "gate:5443", RegistryTokenFile: "/token", WriteQuota: "1Gi", MaxFileSize: "1Gi", TenantQuota: "1Gi"}
	if _, _, err := SetupState(c); err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("%v", err)
	}
	c.RegistryInsecureDev = true
	if _, reg, err := SetupState(c); err != nil || reg == nil {
		t.Fatalf("%v %v", reg, err)
	}
}
