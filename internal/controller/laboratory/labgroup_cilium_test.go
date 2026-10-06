package laboratory

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/cybericebox/laboratory/internal/egress"
	"github.com/cybericebox/laboratory/internal/names"
)

// TestVPNCiliumPolicy asserts the vpn CiliumNetworkPolicy locks egress down to
// kube-apiserver only (never world), and selects the pods by the operator-only component label.
func TestVPNCiliumPolicy(t *testing.T) {
	policy := vpnCiliumPolicy("ns1", map[string]string{names.LabelComponent: "vpn"})

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

	app, found, err := unstructured.NestedString(policy.Object, "spec", "endpointSelector", "matchLabels", names.LabelComponent)
	if err != nil || !found {
		t.Fatalf("endpointSelector.matchLabels.%s not found: err=%v", names.LabelComponent, err)
	}
	if app != "vpn" {
		t.Fatalf("component = %q, want vpn", app)
	}
	if _, has, _ := unstructured.NestedString(policy.Object, "spec", "endpointSelector", "matchLabels", "app"); has {
		t.Fatal("the policy must not select by the app label: a device named vpn carries it")
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
	ingressRule, ok := ingress[0].(map[string]interface{})
	if !ok {
		t.Fatalf("ingress[0] = %T", ingress[0])
	}
	// The source is the proxy's wg-demux (a pod in the proxy namespace), never "world".
	if _, found := ingressRule["fromEntities"]; found {
		t.Fatalf("VPN ingress must not use fromEntities: %#v", ingressRule["fromEntities"])
	}
	from, found, err := unstructured.NestedSlice(ingressRule, "fromEndpoints")
	if err != nil || !found || len(from) != 1 {
		t.Fatalf("VPN ingress fromEndpoints = %#v, err=%v", from, err)
	}
	labels, _, _ := unstructured.NestedStringMap(from[0].(map[string]interface{}), "matchLabels")
	if labels["k8s:io.kubernetes.pod.namespace"] != "laboratory-proxy" || labels["app"] != "laboratory-proxy-l7" {
		t.Fatalf("VPN ingress must come from the proxy pod, got %v", labels)
	}
	toPorts, found, err := unstructured.NestedSlice(ingressRule, "toPorts")
	if err != nil || !found || len(toPorts) != 1 {
		t.Fatalf("VPN ingress ports = %#v, err=%v", toPorts, err)
	}
	portRule := toPorts[0].(map[string]interface{})
	ports, found, err := unstructured.NestedSlice(portRule, "ports")
	if err != nil || !found || len(ports) != 1 {
		t.Fatalf("VPN ingress port set = %#v, err=%v", ports, err)
	}
	port := ports[0].(map[string]interface{})
	if port["port"] != "51820" || port["protocol"] != "UDP" {
		t.Fatalf("VPN probe must not be exposed on public ingress: %#v", port)
	}
}

// The gateway reaches the API server on its API ports only, and the world minus the internal ranges (v4 and v6).
func TestGatewayCiliumPolicy(t *testing.T) {
	policy := gatewayCiliumPolicy("ns1", map[string]string{names.LabelComponent: "gateway"})

	if policy.GetKind() != "CiliumNetworkPolicy" || policy.GetName() != "gateway-egress" || policy.GetNamespace() != "ns1" {
		t.Fatalf("identity: %s %s %s", policy.GetKind(), policy.GetName(), policy.GetNamespace())
	}
	if app, _, _ := unstructured.NestedString(policy.Object, "spec", "endpointSelector", "matchLabels", names.LabelComponent); app != "gateway" {
		t.Fatalf("selects app=%q, want gateway", app)
	}
	egressRules, found, err := unstructured.NestedSlice(policy.Object, "spec", "egress")
	if err != nil || !found || len(egressRules) != 2 {
		t.Fatalf("egress = %v (found %v, err %v), want api + world-minus-internal", egressRules, found, err)
	}

	api := egressRules[0].(map[string]interface{})
	if ents, _, _ := unstructured.NestedStringSlice(api, "toEntities"); len(ents) != 1 || ents[0] != "kube-apiserver" {
		t.Fatalf("first rule must be the API server: %v", api)
	}
	ports, _, _ := unstructured.NestedSlice(api, "toPorts")
	if len(ports) != 1 {
		t.Fatalf("the API server entity needs ports: %v", api)
	}
	var got []string
	for _, p := range ports[0].(map[string]interface{})["ports"].([]interface{}) {
		pm := p.(map[string]interface{})
		got = append(got, pm["port"].(string)+"/"+pm["protocol"].(string))
	}
	if len(got) != 2 || !contains(got, "6443/TCP") || !contains(got, "443/TCP") {
		t.Fatalf("API ports = %v", got)
	}

	world := egressRules[1].(map[string]interface{})
	if _, ok := world["toEntities"]; ok {
		t.Fatal("no entity world: it has no exceptions")
	}
	set, _, _ := unstructured.NestedSlice(world, "toCIDRSet")
	if len(set) != 2 {
		t.Fatalf("world rule: %v", world)
	}
	for i, want := range []struct {
		cidr   string
		except []string
	}{{"0.0.0.0/0", egress.DenyV4}, {"::/0", egress.DenyV6}} {
		entry := set[i].(map[string]interface{})
		if entry["cidr"] != want.cidr {
			t.Fatalf("cidr %v, want %s", entry["cidr"], want.cidr)
		}
		except, _, _ := unstructured.NestedStringSlice(entry, "except")
		if len(except) != len(want.except) {
			t.Fatalf("%s except = %v", want.cidr, except)
		}
	}
	if _, found, _ := unstructured.NestedSlice(policy.Object, "spec", "ingress"); found {
		t.Fatalf("the gateway policy must not define ingress")
	}
}

// The VPN pod reaches the API server on its API ports only.
func TestVPNCiliumPolicyLimitsTheAPIServerPorts(t *testing.T) {
	egress, _, _ := unstructured.NestedSlice(vpnCiliumPolicy("ns1", map[string]string{names.LabelComponent: "vpn"}).Object, "spec", "egress")
	if _, ok := egress[0].(map[string]interface{})["toPorts"]; !ok {
		t.Fatalf("the API server entity of the VPN pod needs ports: %v", egress[0])
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
