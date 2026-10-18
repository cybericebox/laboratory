package laboratory

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/cybericebox/laboratory/internal/names"
)

// TestPrintVPNCiliumPolicy writes the operator's VPN pod policy of a group namespace to a file, for the kind harness of the demux
// (hack/kind-demux/e2e.sh), which applies exactly what the operator would. It runs only when VPN_POLICY_OUT is set:
//
//	VPN_POLICY_NS=lg-a VPN_POLICY_MODE=tc VPN_POLICY_OUT=/tmp/p.yaml go test ./internal/controller/laboratory -run '^TestPrintVPNCiliumPolicy$'
func TestPrintVPNCiliumPolicy(t *testing.T) {
	out := os.Getenv("VPN_POLICY_OUT")
	if out == "" {
		t.Skip("VPN_POLICY_OUT is not set")
	}
	policy := vpnCiliumPolicy(os.Getenv("VPN_POLICY_NS"), map[string]string{names.LabelComponent: names.ComponentVPN},
		names.HostNetworkDemux(os.Getenv("VPN_POLICY_MODE")))
	b, err := yaml.Marshal(policy.Object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
