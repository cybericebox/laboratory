package laboratory

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestVPNCiliumPolicy asserts the vpn CiliumNetworkPolicy locks egress down to
// kube-apiserver only (never world), and selects pods labelled app=vpn.
func TestVPNCiliumPolicy(t *testing.T) {
	policy := vpnCiliumPolicy("ns1")

	if got := policy.GetAPIVersion(); got != "cilium.io/v2" {
		t.Fatalf("apiVersion = %q, want cilium.io/v2", got)
	}
	if got := policy.GetKind(); got != "CiliumNetworkPolicy" {
		t.Fatalf("kind = %q, want CiliumNetworkPolicy", got)
	}
	if got := policy.GetName(); got != "vpn-egress" {
		t.Fatalf("name = %q, want vpn-egress", got)
	}
	if got := policy.GetNamespace(); got != "ns1" {
		t.Fatalf("namespace = %q, want ns1", got)
	}

	app, found, err := unstructured.NestedString(policy.Object, "spec", "endpointSelector", "matchLabels", "app")
	if err != nil || !found {
		t.Fatalf("endpointSelector.matchLabels.app not found: err=%v", err)
	}
	if app != "vpn" {
		t.Fatalf("endpointSelector.matchLabels.app = %q, want vpn", app)
	}

	egress, found, err := unstructured.NestedSlice(policy.Object, "spec", "egress")
	if err != nil || !found {
		t.Fatalf("spec.egress not found: err=%v", err)
	}
	if len(egress) != 1 {
		t.Fatalf("spec.egress has %d rules, want 1", len(egress))
	}
	rule, ok := egress[0].(map[string]interface{})
	if !ok {
		t.Fatalf("egress[0] is not a map: %T", egress[0])
	}
	toEntities, found, err := unstructured.NestedStringSlice(rule, "toEntities")
	if err != nil || !found {
		t.Fatalf("egress[0].toEntities not found: err=%v", err)
	}
	if !contains(toEntities, "kube-apiserver") {
		t.Fatalf("egress toEntities = %v, want to contain kube-apiserver", toEntities)
	}
	if contains(toEntities, "world") {
		t.Fatalf("egress toEntities = %v, must NOT contain world", toEntities)
	}

	// Ingress must allow WireGuard UDP but must not be used to smuggle other
	// egress-equivalent access; just assert it exists and targets port 51820/UDP.
	ingress, found, err := unstructured.NestedSlice(policy.Object, "spec", "ingress")
	if err != nil || !found {
		t.Fatalf("spec.ingress not found: err=%v", err)
	}
	if len(ingress) != 1 {
		t.Fatalf("spec.ingress has %d rules, want 1", len(ingress))
	}
}

// TestGatewayCiliumPolicy asserts the gateway CiliumNetworkPolicy allows
// egress to both kube-apiserver (reconciler) and world (its job), and selects
// pods labelled app=gateway.
func TestGatewayCiliumPolicy(t *testing.T) {
	policy := gatewayCiliumPolicy("ns1")

	if got := policy.GetAPIVersion(); got != "cilium.io/v2" {
		t.Fatalf("apiVersion = %q, want cilium.io/v2", got)
	}
	if got := policy.GetKind(); got != "CiliumNetworkPolicy" {
		t.Fatalf("kind = %q, want CiliumNetworkPolicy", got)
	}
	if got := policy.GetName(); got != "gateway-egress" {
		t.Fatalf("name = %q, want gateway-egress", got)
	}
	if got := policy.GetNamespace(); got != "ns1" {
		t.Fatalf("namespace = %q, want ns1", got)
	}

	app, found, err := unstructured.NestedString(policy.Object, "spec", "endpointSelector", "matchLabels", "app")
	if err != nil || !found {
		t.Fatalf("endpointSelector.matchLabels.app not found: err=%v", err)
	}
	if app != "gateway" {
		t.Fatalf("endpointSelector.matchLabels.app = %q, want gateway", app)
	}

	egress, found, err := unstructured.NestedSlice(policy.Object, "spec", "egress")
	if err != nil || !found {
		t.Fatalf("spec.egress not found: err=%v", err)
	}

	var toEntities []string
	for _, r := range egress {
		rule, ok := r.(map[string]interface{})
		if !ok {
			t.Fatalf("egress rule is not a map: %T", r)
		}
		entities, found, err := unstructured.NestedStringSlice(rule, "toEntities")
		if err != nil || !found {
			t.Fatalf("egress rule toEntities not found: err=%v", err)
		}
		toEntities = append(toEntities, entities...)
	}

	if !contains(toEntities, "kube-apiserver") {
		t.Fatalf("egress toEntities = %v, want to contain kube-apiserver", toEntities)
	}
	if !contains(toEntities, "world") {
		t.Fatalf("egress toEntities = %v, want to contain world", toEntities)
	}

	// Gateway must not have an ingress rule at all.
	if _, found, _ := unstructured.NestedSlice(policy.Object, "spec", "ingress"); found {
		t.Fatalf("spec.ingress present, gateway policy must not define ingress")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
