package chart_test

import (
	"context"
	"net"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// The lab-control pool (operator, agent, lab scheduler, registry) shares a Cilium tunnel with the platform pool
// (internal-docs infrastructure/specs/cluster-layout.md 18.1), so its pods must refuse everything they do not serve: a default-deny ingress in
// its namespaces, and per component only its real callers. The agent is reached in the cluster by the platform's backend (exposure partial,
// the default) and only with exposure open through a public listener, from an allowlist (18.9). The tests below render the chart and judge the
// rendered policies with a small model of Cilium's ingress semantics, so they state who reaches what, not how the YAML is spelled.

// peer is a source of a connection: a pod (namespace and labels) or an address outside the cluster.
type peer struct {
	name   string
	ns     string
	labels map[string]string
	ip     string // set for a source outside the cluster (the world)
}

var (
	backendPeer   = peer{name: "platform backend", ns: "platform", labels: map[string]string{"app": "backend"}}
	frontendPeer  = peer{name: "another platform pod", ns: "platform", labels: map[string]string{"app": "event-frontend"}}
	labPodPeer    = peer{name: "a lab pod", ns: "lg-x", labels: map[string]string{"laboratory.cybericebox.com/device": "d"}}
	worldPeer     = peer{name: "the internet", ip: "203.0.113.7"}
	allowlistPeer = peer{name: "an allowlisted address", ip: "198.51.100.10"}
)

// policy is one rendered CiliumNetworkPolicy or NetworkPolicy, reduced to what decides ingress.
type policy struct {
	kind, name, ns string
	selector       map[string]any // endpointSelector / podSelector
	hasIngress     bool           // the policy enforces ingress on what it selects
	rules          []map[string]any
}

func policiesOf(t *testing.T, out string) []policy {
	t.Helper()
	var res []policy
	for _, d := range docs(t, out) {
		kind, _ := d["kind"].(string)
		if kind != "CiliumNetworkPolicy" && kind != "NetworkPolicy" {
			continue
		}
		md := d["metadata"].(map[string]any)
		spec, _ := d["spec"].(map[string]any)
		p := policy{kind: kind, name: md["name"].(string), ns: md["namespace"].(string)}
		if kind == "CiliumNetworkPolicy" {
			p.selector, _ = spec["endpointSelector"].(map[string]any)
			_, p.hasIngress = spec["ingress"]
		} else {
			p.selector, _ = spec["podSelector"].(map[string]any)
			types, _ := spec["policyTypes"].([]any)
			for _, ty := range types {
				p.hasIngress = p.hasIngress || ty == "Ingress"
			}
		}
		if p.selector == nil {
			t.Fatalf("%s %s/%s selects nothing", kind, p.ns, p.name)
		}
		rules, _ := spec["ingress"].([]any)
		for _, r := range rules {
			m, _ := r.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			p.rules = append(p.rules, m)
		}
		res = append(res, p)
	}
	return res
}

func strip(k string) string { return strings.TrimPrefix(k, "k8s:") }

// selects: a label selector (matchLabels, matchExpressions with Exists) against labels; nsKey is matched against ns.
func selects(t *testing.T, sel map[string]any, ns string, labels map[string]string) bool {
	t.Helper()
	ml, _ := sel["matchLabels"].(map[string]any)
	for k, v := range ml {
		k = strip(k)
		if k == "io.kubernetes.pod.namespace" {
			if v != ns {
				return false
			}
			continue
		}
		if labels[k] != v {
			return false
		}
	}
	me, _ := sel["matchExpressions"].([]any)
	for _, e := range me {
		m := e.(map[string]any)
		if m["operator"] != "Exists" {
			t.Fatalf("selector operator %v is not modelled", m["operator"])
		}
		if _, ok := labels[strip(m["key"].(string))]; !ok {
			return false
		}
	}
	return true
}

func portAllowed(t *testing.T, toPorts any, port string) bool {
	t.Helper()
	list, _ := toPorts.([]any)
	if list == nil {
		return true // no toPorts: every port
	}
	for _, tp := range list {
		ports, _ := tp.(map[string]any)["ports"].([]any)
		for _, p := range ports {
			if pm := p.(map[string]any); pm["port"] == port {
				if pr, _ := pm["protocol"].(string); pr != "" && pr != "TCP" {
					continue
				}
				return true
			}
		}
	}
	return false
}

func npPortAllowed(ports any, port string) bool {
	list, _ := ports.([]any)
	if list == nil {
		return true
	}
	for _, p := range list {
		if v := p.(map[string]any)["port"]; strings.TrimSpace(strings.Trim(yamlScalar(v), "\"")) == port {
			return true
		}
	}
	return false
}

func yamlScalar(v any) string { b, _ := yaml.Marshal(v); return strings.TrimSpace(string(b)) }

func inCIDR(t *testing.T, cidr, ip string) bool {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("a rendered CIDR %q: %v", cidr, err)
	}
	return n.Contains(net.ParseIP(ip))
}

// ciliumRuleAdmits: one CNP ingress rule against a peer. A rule with no source admits nothing (the "ingress: [{}]" default deny).
func ciliumRuleAdmits(t *testing.T, p policy, r map[string]any, src peer, port string) bool {
	t.Helper()
	sources := 0
	ok := false
	if eps, has := r["fromEndpoints"].([]any); has {
		sources++
		for _, e := range eps {
			sel, _ := e.(map[string]any)
			if src.ip != "" {
				continue
			}
			// a selector without the namespace key matches the policy's own namespace only
			ns := p.ns
			if ml, _ := sel["matchLabels"].(map[string]any); ml != nil {
				for k, v := range ml {
					if strip(k) == "io.kubernetes.pod.namespace" {
						ns, _ = v.(string)
					}
				}
			}
			if ns == src.ns && selects(t, sel, src.ns, src.labels) {
				ok = true
			}
		}
	}
	if ents, has := r["fromEntities"].([]any); has {
		sources++
		for _, e := range ents {
			switch e {
			case "all":
				ok = true
			case "world":
				ok = ok || src.ip != ""
			case "cluster":
				ok = ok || src.ip == ""
			case "host", "remote-node", "kube-apiserver":
			default:
				t.Fatalf("entity %v is not modelled", e)
			}
		}
	}
	if cidrs, has := r["fromCIDR"].([]any); has {
		sources++
		for _, c := range cidrs {
			ok = ok || (src.ip != "" && inCIDR(t, c.(string), src.ip))
		}
	}
	for k := range r {
		if k != "fromEndpoints" && k != "fromEntities" && k != "fromCIDR" && k != "toPorts" {
			t.Fatalf("%s/%s: ingress field %s is not modelled", p.ns, p.name, k)
		}
	}
	if sources == 0 {
		if _, has := r["toPorts"]; has {
			t.Fatalf("%s/%s: a rule with ports and no source: %v", p.ns, p.name, r)
		}
		return false
	}
	return ok && portAllowed(t, r["toPorts"], port)
}

func npRuleAdmits(t *testing.T, p policy, r map[string]any, src peer, port string) bool {
	t.Helper()
	if !npPortAllowed(r["ports"], port) {
		return false
	}
	from, has := r["from"].([]any)
	if !has {
		return true // no from: every source
	}
	for _, f := range from {
		fm := f.(map[string]any)
		if b, ok := fm["ipBlock"].(map[string]any); ok {
			if src.ip != "" && inCIDR(t, b["cidr"].(string), src.ip) {
				return true
			}
			continue
		}
		t.Fatalf("%s/%s: NetworkPolicy peer %v is not modelled", p.ns, p.name, fm)
	}
	return false
}

// admits decides whether src reaches the pod (ns, labels) on port: with no policy that enforces ingress the pod is open; otherwise any rule
// of any selecting policy must admit (allow rules only, as the chart writes them).
func admits(t *testing.T, pols []policy, ns string, labels map[string]string, src peer, port string) bool {
	t.Helper()
	enforced := false
	for _, p := range pols {
		if p.ns != ns || !p.hasIngress || !selects(t, p.selector, ns, labels) {
			continue
		}
		enforced = true
		for _, r := range p.rules {
			if p.kind == "CiliumNetworkPolicy" && ciliumRuleAdmits(t, p, r, src, port) {
				return true
			}
			if p.kind == "NetworkPolicy" && npRuleAdmits(t, p, r, src, port) {
				return true
			}
		}
	}
	return !enforced
}

// target is a pod of the lab-control pool, with the labels the chart renders on it.
type target struct {
	ns     string
	labels map[string]string
}

func labControlTargets(t *testing.T, out string) map[string]target {
	t.Helper()
	res := map[string]target{}
	for _, d := range docs(t, out) {
		if d["kind"] != "Deployment" {
			continue
		}
		md := d["metadata"].(map[string]any)
		tpl := d["spec"].(map[string]any)["template"].(map[string]any)
		lm, _ := tpl["metadata"].(map[string]any)["labels"].(map[string]any)
		labels := map[string]string{}
		for k, v := range lm {
			labels[k], _ = v.(string)
		}
		res[md["name"].(string)] = target{ns: md["namespace"].(string), labels: labels}
	}
	for _, name := range []string{"laboratory-controller-manager", "laboratory-agent", "laboratory-scheduler", "laboratory-registry"} {
		if _, ok := res[name]; !ok {
			t.Fatalf("no Deployment %s rendered", name)
		}
	}
	return res
}

type reach struct {
	pod  string
	src  peer
	port string
	want bool
}

func checkReach(t *testing.T, what string, out string, cases []reach) {
	t.Helper()
	pols := policiesOf(t, out)
	pods := labControlTargets(t, out)
	for _, c := range cases {
		tg := pods[c.pod]
		if got := admits(t, pols, tg.ns, tg.labels, c.src, c.port); got != c.want {
			t.Errorf("%s: %s -> %s:%s admitted %v, want %v", what, c.src.name, c.pod, c.port, got, c.want)
		}
	}
}

var labControlSet = append([]string{"--set", "agent.enabled=true", "--set", "agent.domain=agent.example.com"}, everything...)

const (
	lcAgent, lcOperator, lcScheduler, lcRegistry = "laboratory-agent", "laboratory-controller-manager", "laboratory-scheduler", "laboratory-registry"
)

// partial (the default): the backend reaches the agent's gRPC port and nothing else of the pool; nobody else reaches anything.
func TestLabControlIngressPartial(t *testing.T) {
	out, err := helmTemplate(t, labControlSet...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var cases []reach
	for _, src := range []peer{frontendPeer, labPodPeer, worldPeer, allowlistPeer} {
		cases = append(cases, reach{lcAgent, src, "5454", false})
	}
	cases = append(cases, reach{lcAgent, backendPeer, "5454", true}, reach{lcAgent, backendPeer, "9443", false})
	for _, src := range []peer{backendPeer, frontendPeer, labPodPeer, worldPeer} {
		cases = append(cases,
			reach{lcOperator, src, "8081", false}, reach{lcOperator, src, "8080", false},
			reach{lcScheduler, src, "10259", false},
			reach{lcRegistry, src, "5443", false}, reach{lcRegistry, src, "5002", false})
	}
	checkReach(t, "partial", out, cases)
	// closed changes nothing for the agent
	closed, err := helmTemplate(t, append(append([]string{}, labControlSet...), "--set", "exposure=closed")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, closed)
	}
	checkReach(t, "closed", closed, []reach{{lcAgent, backendPeer, "5454", true}, {lcAgent, worldPeer, "5454", false}, {lcAgent, frontendPeer, "5454", false}})
}

// open: the allowlist through the public listener, on the gRPC port only; the backend keeps its in-cluster path.
func TestLabControlIngressOpen(t *testing.T) {
	out, err := helmTemplate(t, append(append([]string{}, labControlSet...), "--set", "exposure=open",
		"--set", "agent.networkPolicy.ingressFrom.fromCIDR[0]=198.51.100.0/24")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	checkReach(t, "open", out, []reach{
		{lcAgent, allowlistPeer, "5454", true}, {lcAgent, backendPeer, "5454", true},
		{lcAgent, worldPeer, "5454", false}, {lcAgent, frontendPeer, "5454", false}, {lcAgent, labPodPeer, "5454", false},
		{lcAgent, allowlistPeer, "9443", false},
		{lcOperator, allowlistPeer, "8081", false}, {lcScheduler, allowlistPeer, "10259", false}, {lcRegistry, allowlistPeer, "5443", false},
	})
}

// The metrics ports, when on, are for the cluster only: never the world (the agent's plain NetworkPolicy once opened its metrics port to any source).
func TestLabControlMetricsFromTheClusterOnly(t *testing.T) {
	out, err := helmTemplate(t, append(append([]string{}, labControlSet...), "--set", "agent.metrics.enabled=true", "--set", "operator.metrics.enabled=true")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	checkReach(t, "metrics", out, []reach{
		{lcAgent, frontendPeer, "9443", true}, {lcAgent, worldPeer, "9443", false}, {lcAgent, frontendPeer, "5454", false},
		{lcOperator, frontendPeer, "8080", true}, {lcOperator, worldPeer, "8080", false}, {lcOperator, frontendPeer, "8081", false},
	})
}

// A custom caller set replaces the default backend.
func TestAgentCallersAreAValue(t *testing.T) {
	out, err := helmTemplate(t, append(append([]string{}, labControlSet...),
		"--set", "agent.networkPolicy.ingressFrom.fromEndpoints[0].k8s:io\\.kubernetes\\.pod\\.namespace=apps",
		"--set", "agent.networkPolicy.ingressFrom.fromEndpoints[0].app=api")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	api := peer{name: "apps/api", ns: "apps", labels: map[string]string{"app": "api"}}
	checkReach(t, "custom callers", out, []reach{{lcAgent, api, "5454", true}, {lcAgent, backendPeer, "5454", false}})
}

// Every pod of the lab-control namespaces that is on the pod network is selected by a policy that enforces ingress: a pod added later without
// a policy of its own is closed, not open.
func TestLabControlNamespacesDenyIngressByDefault(t *testing.T) {
	out, err := helmTemplate(t, labControlSet...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pols := policiesOf(t, out)
	for _, ns := range []string{"laboratory-system", "laboratory-agent"} {
		var deny *policy
		for i, p := range pols {
			if p.ns == ns && p.kind == "CiliumNetworkPolicy" && len(p.selector) == 0 {
				deny = &pols[i]
			}
		}
		if deny == nil || !deny.hasIngress || len(deny.rules) != 1 || len(deny.rules[0]) != 0 {
			t.Errorf("%s: no namespace-wide default-deny ingress (endpointSelector {}, ingress [{}]): %+v", ns, deny)
		}
		// an unknown pod of the namespace is closed
		if admits(t, pols, ns, map[string]string{"app": "something-new"}, backendPeer, "80") {
			t.Errorf("%s: a pod without its own policy is reachable", ns)
		}
	}
	for key, spec := range podsOf(t, out) {
		parts := strings.SplitN(key, "/", 3)
		if parts[1] != "laboratory-system" && parts[1] != "laboratory-agent" || spec.HostNetwork {
			continue
		}
		for _, d := range docs(t, out) {
			md := d["metadata"].(map[string]any)
			if d["kind"] != parts[0] || md["name"] != parts[2] {
				continue
			}
			lm := d["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
			labels := map[string]string{}
			for k, v := range lm {
				labels[k], _ = v.(string)
			}
			if admits(t, pols, parts[1], labels, frontendPeer, "1") {
				t.Errorf("%s: open to a platform pod", key)
			}
		}
	}
	// The default deny is ingress only: it must not cut the egress the components need (Cilium enforces egress only where a policy has egress rules).
	for _, d := range docs(t, out) {
		if d["kind"] != "CiliumNetworkPolicy" {
			continue
		}
		spec := d["spec"].(map[string]any)
		if sel, _ := spec["endpointSelector"].(map[string]any); len(sel) == 0 {
			if _, ok := spec["egress"]; ok {
				t.Errorf("%v: a namespace-wide policy with egress rules", d["metadata"])
			}
		}
	}
	// without the policies switched on, nothing of the default deny is rendered
	off, err := helmTemplate(t, "--set", "agent.enabled=true", "--set", "agent.domain=agent.example.com", "--set", "operator.networkPolicy.enabled=false",
		"--set", "agent.networkPolicy.enabled=false", "--set", "labScheduler.networkPolicy.enabled=false")
	if err != nil {
		t.Fatalf("%v\n%s", err, off)
	}
	for _, p := range policiesOf(t, off) {
		if p.ns != "laboratory-proxy" && p.ns != "laboratory-wg" && p.ns != "laboratory-device-access" && p.name != "laboratory-registry" && p.name != "laboratory-checker" {
			t.Errorf("policy %s/%s rendered with the lab-control policies off", p.ns, p.name)
		}
	}
}

// The agent's Service: ClusterIP in the cluster (partial, closed); a NodePort with Local traffic for the public listener only with open. The
// API server accepts each (envtest).
func TestAgentServiceFollowsExposure(t *testing.T) {
	cases := []struct {
		set  []string
		want corev1.ServiceType
	}{
		{nil, corev1.ServiceTypeClusterIP},
		{[]string{"--set", "exposure=partial"}, corev1.ServiceTypeClusterIP},
		{[]string{"--set", "exposure=closed"}, corev1.ServiceTypeClusterIP},
		{[]string{"--set", "exposure=open", "--set", "agent.networkPolicy.ingressFrom.fromCIDR[0]=198.51.100.0/24", "--set", "agent.service.nodePort=30454"}, corev1.ServiceTypeNodePort},
	}
	var svcs []corev1.Service
	for _, c := range cases {
		var svc corev1.Service
		render(t, "templates/agent/service.yaml", &svc, append(append([]string{}, agentSet...), c.set...)...)
		if svc.Spec.Type != c.want {
			t.Errorf("%v: type %q, want %q", c.set, svc.Spec.Type, c.want)
		}
		if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 5454 {
			t.Errorf("%v: ports %+v", c.set, svc.Spec.Ports)
		}
		if c.want == corev1.ServiceTypeClusterIP && (svc.Spec.ExternalTrafficPolicy != "" || svc.Spec.Ports[0].NodePort != 0) {
			t.Errorf("%v: a ClusterIP Service with node-port fields: %+v", c.set, svc.Spec)
		}
		if c.want == corev1.ServiceTypeNodePort && (svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal || svc.Spec.Ports[0].NodePort != 30454) {
			t.Errorf("%v: the public listener's Service: %+v", c.set, svc.Spec)
		}
		svcs = append(svcs, svc)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: envtestDir()}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "laboratory-agent"}}); err != nil {
		t.Fatal(err)
	}
	for i, svc := range svcs {
		if err := c.Create(ctx, &svc); err != nil {
			t.Errorf("%v: the API server refuses the Service: %v", cases[i].set, err)
			continue
		}
		if err := c.Delete(ctx, &svc); err != nil {
			t.Fatal(err)
		}
	}
}

// Values that would open the agent wider than the mode says, or to nobody, are refused at install time.
func TestAgentExposureValuesAreChecked(t *testing.T) {
	open := []string{"--set", "exposure=open"}
	cidr := []string{"--set", "agent.networkPolicy.ingressFrom.fromCIDR[0]=198.51.100.0/24"}
	join := func(parts ...[]string) []string {
		res := append([]string{}, agentSet...)
		for _, p := range parts {
			res = append(res, p...)
		}
		return res
	}
	for _, c := range []struct {
		name string
		set  []string
		msg  string
	}{
		{"unknown mode", join([]string{"--set", "exposure=public"}), "exposure must be closed, partial or open"},
		{"open without an allowlist", join(open), "agent.networkPolicy.ingressFrom.fromCIDR"},
		{"an allowlist without open", join(cidr), "only with exposure=open"},
		{"a node port without open", join([]string{"--set", "agent.service.nodePort=30454"}), "agent.service.nodePort"},
		{"the whole internet", join(open, []string{"--set", "agent.networkPolicy.ingressFrom.fromCIDR[0]=0.0.0.0/0"}), "the whole address space"},
		{"not a CIDR", join(open, []string{"--set", "agent.networkPolicy.ingressFrom.fromCIDR[0]=198.51.100.10"}), "not a CIDR"},
		{"no caller at all", join([]string{"--set-json", "agent.networkPolicy.ingressFrom.fromEndpoints=[]"}), "no caller"},
		{"a caller of every namespace", join([]string{"--set-json", "agent.networkPolicy.ingressFrom.fromEndpoints=[{\"app\":\"backend\"}]"}), "k8s:io.kubernetes.pod.namespace"},
	} {
		out, err := helmTemplate(t, c.set...)
		if err == nil || !strings.Contains(out, c.msg) {
			t.Errorf("%s: err %v, want a refusal naming %q:\n%s", c.name, err, c.msg, out)
		}
	}
	for _, ok := range [][]string{join(), join(open, cidr), join(open, cidr, []string{"--set", "agent.service.nodePort=30454"})} {
		if out, err := helmTemplate(t, ok...); err != nil {
			t.Errorf("%v refused: %v\n%s", ok, err, out)
		}
	}
}
