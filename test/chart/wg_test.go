package chart_test

import (
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

func wgDemux(t *testing.T, extra ...string) (appsv1.DaemonSet, corev1.Container) {
	t.Helper()
	var ds appsv1.DaemonSet
	render(t, "templates/wg/daemonset.yaml", &ds, extra...)
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == "wg-demux" {
			return ds, c
		}
	}
	t.Fatalf("no wg-demux container in %+v", ds.Spec.Template.Spec.Containers)
	return ds, corev1.Container{}
}

func wgNamespaceLevel(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := helmTemplate(t, append(extra, "-s", "templates/wg/namespace.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for name, ns := range ofKind(docs(t, out), "Namespace") {
		if name != "laboratory-wg" {
			t.Errorf("namespace %s", name)
		}
		labels, _ := ns["metadata"].(map[string]any)["labels"].(map[string]any)
		level, _ := labels["pod-security.kubernetes.io/enforce"].(string)
		return level
	}
	t.Fatal("no namespace rendered")
	return ""
}

// The tc mode: the demux runs on the host network of every entry node with the three capabilities the BPF program needs.
func TestWGDemuxTCDaemonSet(t *testing.T) {
	ds, c := wgDemux(t, "--set", "proxy.wg.mode=tc")
	if ds.Namespace != "laboratory-wg" || ds.Name != "laboratory-proxy-wg" {
		t.Errorf("identity: %s/%s", ds.Namespace, ds.Name)
	}
	spec := ds.Spec.Template.Spec
	if !spec.HostNetwork || spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Errorf("hostNetwork with ClusterFirstWithHostNet: %v %v", spec.HostNetwork, spec.DNSPolicy)
	}
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil || *spec.SecurityContext.RunAsNonRoot || spec.SecurityContext.RunAsUser == nil || *spec.SecurityContext.RunAsUser != 0 {
		t.Errorf("the BPF program is loaded by root: %+v", spec.SecurityContext)
	}
	sc := c.SecurityContext
	if sc == nil || sc.Capabilities == nil {
		t.Fatalf("no security context: %+v", sc)
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("never privileged")
	}
	if len(sc.Capabilities.Add) != 3 || !hasCap(sc.Capabilities.Add, "BPF") || !hasCap(sc.Capabilities.Add, "NET_ADMIN") || !hasCap(sc.Capabilities.Add, "PERFMON") {
		t.Errorf("caps add exactly BPF, NET_ADMIN, PERFMON: %v", sc.Capabilities.Add)
	}
	if len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("caps drop ALL: %v", sc.Capabilities.Drop)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("no privilege escalation, read-only root: %+v", sc)
	}
	var mounted bool
	for _, m := range c.VolumeMounts {
		mounted = mounted || (m.Name == "bpffs" && m.MountPath == "/sys/fs/bpf")
	}
	var hostPath *corev1.HostPathVolumeSource
	for _, v := range spec.Volumes {
		if v.Name == "bpffs" {
			hostPath = v.HostPath
		}
	}
	if !mounted || hostPath == nil || hostPath.Path != "/sys/fs/bpf" || hostPath.Type == nil || *hostPath.Type != corev1.HostPathDirectory {
		t.Errorf("/sys/fs/bpf is a hostPath Directory mounted at /sys/fs/bpf: %v %+v", mounted, hostPath)
	}
	env := envOf(c)
	if env["DEMUX_MODE"].Value != "tc" {
		t.Errorf("DEMUX_MODE = %q", env["DEMUX_MODE"].Value)
	}
	if env["NODE_NAME"].ValueFrom == nil || env["NODE_NAME"].ValueFrom.FieldRef.FieldPath != "spec.nodeName" {
		t.Errorf("NODE_NAME: %+v", env["NODE_NAME"])
	}
	if env["HOST_IP"].ValueFrom == nil || env["HOST_IP"].ValueFrom.FieldRef.FieldPath != "status.hostIP" {
		t.Errorf("HOST_IP: %+v", env["HOST_IP"])
	}
	if env["DEMUX_SERVICE_NAME"].Value != "laboratory-proxy-wg" || env["DEMUX_SERVICE_NAMESPACE"].Value != "laboratory-wg" {
		t.Errorf("service: %q %q", env["DEMUX_SERVICE_NAME"].Value, env["DEMUX_SERVICE_NAMESPACE"].Value)
	}
	ports := map[string]int32{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}
	if ports["wg"] != 51820 || ports["health"] != 8082 || ports["metrics"] != 8083 {
		t.Errorf("ports: %v", ports)
	}
	if r := ds.Spec.UpdateStrategy.RollingUpdate; r == nil || r.MaxUnavailable.IntValue() != 1 || ds.Spec.MinReadySeconds != 20 {
		t.Errorf("rolling update one node at a time: %+v %d", ds.Spec.UpdateStrategy, ds.Spec.MinReadySeconds)
	}
	if lvl := wgNamespaceLevel(t, "--set", "proxy.wg.mode=tc"); lvl != "privileged" {
		t.Errorf("the tc namespace must admit the privileged level: %q", lvl)
	}
	for _, mode := range []string{"xdp"} {
		_, c := wgDemux(t, "--set", "proxy.wg.mode="+mode)
		if envOf(c)["DEMUX_MODE"].Value != mode || len(c.SecurityContext.Capabilities.Add) != 3 {
			t.Errorf("%s: %+v", mode, c.SecurityContext)
		}
		if lvl := wgNamespaceLevel(t, "--set", "proxy.wg.mode="+mode); lvl != "privileged" {
			t.Errorf("%s namespace: %q", mode, lvl)
		}
	}
}

// userspace keeps the old unprivileged pod: pod network, non-root, nothing from the host.
func TestWGDemuxUserspaceIsUnprivileged(t *testing.T) {
	ds, c := wgDemux(t, "--set", "proxy.wg.mode=userspace")
	spec := ds.Spec.Template.Spec
	if spec.HostNetwork {
		t.Error("no hostNetwork in userspace")
	}
	if sc := spec.SecurityContext; sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 65532 || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context: %+v", sc)
	}
	sc := c.SecurityContext
	if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 || !dropsAllCaps(sc.Capabilities.Drop) {
		t.Errorf("drop ALL, add nothing: %+v", sc)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("hardened: %+v", sc)
	}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			t.Errorf("no hostPath in userspace: %+v", v)
		}
	}
	if len(c.VolumeMounts) != 0 {
		t.Errorf("no mounts: %+v", c.VolumeMounts)
	}
	if envOf(c)["DEMUX_MODE"].Value != "userspace" {
		t.Errorf("DEMUX_MODE = %q", envOf(c)["DEMUX_MODE"].Value)
	}
	if lvl := wgNamespaceLevel(t, "--set", "proxy.wg.mode=userspace"); lvl != "restricted" {
		t.Errorf("the userspace namespace is restricted: %q", lvl)
	}
}

func dropsAllCaps(drop []corev1.Capability) bool { return len(drop) == 1 && drop[0] == "ALL" }

func TestWGModeValidated(t *testing.T) {
	out, err := helmTemplate(t, "--set", "proxy.wg.mode=foo", "-s", "templates/wg/daemonset.yaml")
	if err == nil || !strings.Contains(out, "proxy.wg.mode") {
		t.Fatalf("an unknown mode must be refused: %v\n%s", err, out)
	}
	out, err = helmTemplate(t, "--set", "proxy.wg.externalTrafficPolicy=Both", "-s", "templates/wg/service.yaml")
	if err == nil || !strings.Contains(out, "externalTrafficPolicy") {
		t.Fatalf("an unknown policy must be refused: %v\n%s", err, out)
	}
}

func TestWGServicePolicyFollowsMode(t *testing.T) {
	var svc corev1.Service
	render(t, "templates/wg/service.yaml", &svc)
	if svc.Namespace != "laboratory-wg" || svc.Name != "laboratory-proxy-wg" {
		t.Errorf("identity: %s/%s", svc.Namespace, svc.Name)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster {
		t.Errorf("userspace (default) ETP = %q, want Cluster", svc.Spec.ExternalTrafficPolicy)
	}
	for _, mode := range []string{"tc", "xdp"} {
		var fast corev1.Service
		render(t, "templates/wg/service.yaml", &fast, "--set", "proxy.wg.mode="+mode)
		if fast.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
			t.Errorf("%s ETP = %q, want Local", mode, fast.Spec.ExternalTrafficPolicy)
		}
	}
	if svc.Spec.Selector["app"] != "laboratory-proxy-wg" {
		t.Errorf("selector: %v", svc.Spec.Selector)
	}
	if svc.Annotations["lbipam.cilium.io/sharing-cross-namespace"] != "laboratory-system" || svc.Annotations["lbipam.cilium.io/sharing-key"] != "laboratory-edge" {
		t.Errorf("annotations: %v", svc.Annotations)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 51820 || svc.Spec.Ports[0].TargetPort.IntValue() != 51820 || svc.Spec.Ports[0].Protocol != corev1.ProtocolUDP {
		t.Errorf("ports: %+v", svc.Spec.Ports)
	}
	var cluster corev1.Service
	render(t, "templates/wg/service.yaml", &cluster, "--set", "proxy.wg.mode=tc", "--set", "proxy.wg.externalTrafficPolicy=Cluster")
	if cluster.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster {
		t.Errorf("tc with Cluster set: ETP = %q", cluster.Spec.ExternalTrafficPolicy)
	}
	var local corev1.Service
	render(t, "templates/wg/service.yaml", &local, "--set", "proxy.wg.externalTrafficPolicy=Local")
	if local.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("userspace with Local set: ETP = %q", local.Spec.ExternalTrafficPolicy)
	}
}

// The Gateway and the VPN Service name each other's namespace for IP sharing.
func TestGatewayShareNamesTheWGNamespace(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/gateway/gateway.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, `lbipam.cilium.io/sharing-cross-namespace: "laboratory-wg"`) {
		t.Errorf("the Gateway must name laboratory-wg:\n%s", out)
	}
}

// l7 is alone in its pod, unprivileged, in a restricted namespace, in both of its modes.
func TestL7PodHasNoDemux(t *testing.T) {
	for _, mode := range []string{"replicas", "daemonset"} {
		out, err := helmTemplate(t, "--set", "proxy.mode="+mode, "-s", "templates/proxy/"+map[string]string{"replicas": "deployment", "daemonset": "daemonset"}[mode]+".yaml")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		specs := podSpecs(t, out)
		if len(specs) != 1 {
			t.Fatalf("%s: %v", mode, specs)
		}
		for name, spec := range specs {
			if spec["hostNetwork"] == true {
				t.Errorf("%s %s: no hostNetwork", mode, name)
			}
			cs := containersOf(spec, "containers")
			if len(cs) != 1 || cs[0]["name"] != "l7" {
				t.Errorf("%s %s: l7 only: %v", mode, name, cs)
			}
			if strings.Contains(out, "wg-demux") || strings.Contains(out, "DEMUX_") {
				t.Errorf("%s: no demux in the l7 pod", mode)
			}
		}
	}
	out, err := helmTemplate(t, "-s", "templates/proxy/namespace.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "pod-security.kubernetes.io/enforce: restricted") {
		t.Errorf("laboratory-proxy stays restricted:\n%s", out)
	}
}

// The demux reads the LabGroups and the Nodes, its own Service, and writes Events in the release namespace; nothing else.
func TestWGDemuxRBAC(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/wg/clusterrole.yaml", "-s", "templates/wg/clusterrolebinding.yaml", "-s", "templates/wg/role.yaml", "-s", "templates/wg/serviceaccount.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	all := docs(t, out)
	var cr map[string]any
	for _, d := range all {
		md := d["metadata"].(map[string]any)
		if d["kind"] == "ClusterRole" {
			cr = d
		}
		if d["kind"] == "ServiceAccount" && (md["name"] != "laboratory-proxy-wg" || md["namespace"] != "laboratory-wg") {
			t.Errorf("service account: %v", md)
		}
	}
	if cr == nil {
		t.Fatal("no ClusterRole")
	}
	got := map[string]bool{}
	for _, r := range rulesOf(t, cr) {
		for _, res := range r.Resources {
			got[res] = true
			if len(r.Verbs) != 3 || !has(r.Verbs, "get") || !has(r.Verbs, "list") || !has(r.Verbs, "watch") {
				t.Errorf("%s: read-only: %v", res, r.Verbs)
			}
		}
	}
	if len(got) != 2 || !got["labgroups"] || !got["nodes"] {
		t.Errorf("cluster rights: %v", got)
	}
	wantRef := map[string]string{"ClusterRoleBinding/laboratory-proxy-wg": "ClusterRole/laboratory-proxy-wg", "RoleBinding/laboratory-proxy-wg": "Role/laboratory-proxy-wg", "RoleBinding/laboratory-proxy-wg-events": "Role/laboratory-proxy-wg-events"}
	seen := 0
	for _, d := range all {
		kind, _ := d["kind"].(string)
		if kind != "RoleBinding" && kind != "ClusterRoleBinding" {
			continue
		}
		name := d["metadata"].(map[string]any)["name"].(string)
		ref := d["roleRef"].(map[string]any)
		if want, ok := wantRef[kind+"/"+name]; !ok || ref["kind"].(string)+"/"+ref["name"].(string) != want {
			t.Errorf("%s/%s roleRef %v", kind, name, ref)
		}
		subs := d["subjects"].([]any)
		if len(subs) != 1 || subs[0].(map[string]any)["name"] != "laboratory-proxy-wg" || subs[0].(map[string]any)["namespace"] != "laboratory-wg" || subs[0].(map[string]any)["kind"] != "ServiceAccount" {
			t.Errorf("%s/%s subjects %v", kind, name, subs)
		}
		seen++
	}
	if seen != 3 {
		t.Errorf("%d bindings", seen)
	}
	roles := ofKind(all, "Role")
	svc, ev := roles["laboratory-proxy-wg"], roles["laboratory-proxy-wg-events"]
	if svc == nil || ev == nil {
		t.Fatalf("roles: %v", roles)
	}
	if svc["metadata"].(map[string]any)["namespace"] != "laboratory-wg" {
		t.Errorf("the services role lives in laboratory-wg: %v", svc["metadata"])
	}
	if rs := rulesOf(t, svc); len(rs) != 1 || !has(rs[0].Resources, "services") || len(rs[0].Verbs) != 3 {
		t.Errorf("services get/list/watch: %+v", rs)
	}
	if ev["metadata"].(map[string]any)["namespace"] != "laboratory-system" {
		t.Errorf("the events role lives in the release namespace: %v", ev["metadata"])
	}
	if rs := rulesOf(t, ev); len(rs) != 1 || !has(rs[0].Resources, "events") || !has(rs[0].Verbs, "create") || !has(rs[0].Verbs, "patch") || len(rs[0].Verbs) != 2 {
		t.Errorf("events create/patch: %+v", rs)
	}
}

func TestWGOperatorKnowsTheMode(t *testing.T) {
	out, err := helmTemplate(t, "--set", "proxy.wg.mode=tc", "-s", "templates/operator/configmap.yaml")
	if err != nil || !strings.Contains(out, `WG_DEMUX_MODE: "tc"`) {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = helmTemplate(t, "--set", "proxy.wg.mode=userspace", "-s", "templates/operator/configmap.yaml")
	if err != nil || !strings.Contains(out, `WG_DEMUX_MODE: "userspace"`) {
		t.Fatalf("%v\n%s", err, out)
	}
}

// With the demux off the operator must not admit node addresses to the VPN pods.
func TestWGOperatorModeWithoutDemux(t *testing.T) {
	out, err := helmTemplate(t, "--set", "proxy.wg.enabled=false", "-s", "templates/operator/configmap.yaml")
	if err != nil || !strings.Contains(out, `WG_DEMUX_MODE: "userspace"`) {
		t.Fatalf("%v\n%s", err, out)
	}
}

// tc and xdp bind the probe and metrics ports to the node address, so the node's other interfaces do not expose them.
func TestWGHostNetworkBindsToHostIP(t *testing.T) {
	for _, mode := range []string{"tc", "xdp"} {
		_, c := wgDemux(t, "--set", "proxy.wg.mode="+mode)
		env := envOf(c)
		if env["HEALTH_ADDR"].Value != "$(HOST_IP):8082" || env["DEMUX_METRICS_ADDR"].Value != "$(HOST_IP):8083" {
			t.Errorf("%s: %q %q", mode, env["HEALTH_ADDR"].Value, env["DEMUX_METRICS_ADDR"].Value)
		}
		pos := map[string]int{}
		for i, e := range c.Env {
			pos[e.Name] = i
		}
		if pos["HOST_IP"] > pos["HEALTH_ADDR"] || pos["HOST_IP"] > pos["DEMUX_METRICS_ADDR"] {
			t.Errorf("%s: HOST_IP must be defined before it is expanded", mode)
		}
		if c.ReadinessProbe.HTTPGet.Port.String() != "health" {
			t.Errorf("%s: probes use the named port", mode)
		}
	}
	_, c := wgDemux(t, "--set", "proxy.wg.mode=userspace")
	if _, set := envOf(c)["HEALTH_ADDR"]; set {
		t.Error("userspace keeps the image defaults")
	}
	if _, set := envOf(c)["DEMUX_METRICS_ADDR"]; set {
		t.Error("userspace keeps the image defaults")
	}
}

func TestWGDemuxNetworkPolicy(t *testing.T) {
	out, err := helmTemplate(t, "--set", "proxy.wg.mode=userspace", "-s", "templates/netpol/wg-demux.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var np networkingv1.NetworkPolicy
	var cnp map[string]any
	for _, d := range strings.Split(out, "\n---\n") {
		if strings.Contains(d, "kind: NetworkPolicy") {
			if err := yaml.Unmarshal([]byte(d), &np); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Contains(d, "kind: CiliumNetworkPolicy") {
			if err := yaml.Unmarshal([]byte(d), &cnp); err != nil {
				t.Fatal(err)
			}
		}
	}
	if np.Namespace != "laboratory-wg" || np.Spec.PodSelector.MatchLabels["app"] != "laboratory-proxy-wg" || cnp == nil {
		t.Fatalf("policies: %+v %v", np, cnp)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].Ports) != 1 || np.Spec.Ingress[0].Ports[0].Port.IntValue() != 51820 || len(np.Spec.Ingress[0].From) != 0 {
		t.Errorf("vanilla ingress: UDP 51820 from all: %+v", np.Spec.Ingress)
	}
	if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].Ports) != 2 {
		t.Errorf("vanilla egress: kube-dns only: %+v", np.Spec.Egress)
	}
	spec := cnp["spec"].(map[string]any)
	if cnp["metadata"].(map[string]any)["namespace"] != "laboratory-wg" || spec["endpointSelector"].(map[string]any)["matchLabels"].(map[string]any)["app"] != "laboratory-proxy-wg" {
		t.Errorf("cilium identity: %v", cnp["metadata"])
	}
	ing := spec["ingress"].([]any)
	if len(ing) != 2 {
		t.Fatalf("ingress: %v", ing)
	}
	all := ing[0].(map[string]any)
	if fmt.Sprint(all["fromEntities"]) != "[all]" || fmt.Sprint(all["toPorts"]) != "[map[ports:[map[port:51820 protocol:UDP]]]]" {
		t.Errorf("public UDP 51820: %v", all)
	}
	host := ing[1].(map[string]any)
	if fmt.Sprint(host["fromEntities"]) != "[host]" || fmt.Sprint(host["toPorts"]) != "[map[ports:[map[port:8082 protocol:TCP] map[port:8083 protocol:TCP]]]]" {
		t.Errorf("health and metrics from the host: %v", host)
	}
	eg := spec["egress"].([]any)
	if len(eg) != 3 {
		t.Fatalf("egress: %v", eg)
	}
	if fmt.Sprint(eg[0]) != "map[toEntities:[kube-apiserver]]" {
		t.Errorf("apiserver: %v", eg[0])
	}
	if !strings.Contains(fmt.Sprint(eg[1]), "k8s-app:kube-dns") || !strings.Contains(fmt.Sprint(eg[1]), "53") {
		t.Errorf("dns: %v", eg[1])
	}
	grp := fmt.Sprint(eg[2])
	if !strings.Contains(grp, "laboratory.cybericebox.com/group") || !strings.Contains(grp, "Exists") || !strings.Contains(grp, "port:51820 protocol:UDP") {
		t.Errorf("group endpoints on UDP 51820: %v", grp)
	}

	for _, extra := range [][]string{{"--set", "proxy.wg.mode=tc"}, {"--set", "proxy.wg.mode=xdp"}, {"--set", "proxy.wg.mode=userspace", "--set", "proxy.networkPolicy.enabled=false"}, {"--set", "proxy.wg.mode=userspace", "--set", "proxy.wg.enabled=false"}} {
		if o, err := helmTemplate(t, append(extra, "-s", "templates/netpol/wg-demux.yaml")...); err == nil && strings.Contains(o, "kind:") {
			t.Errorf("%v: no pod policy expected:\n%s", extra, o)
		}
	}
}

// l7 has no WireGuard rule: the demux has its own policy.
func TestL7PolicyHasNoWireGuard(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/netpol/proxy-l7.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "51820") {
		t.Errorf("no WireGuard port in the l7 policy:\n%s", out)
	}
	if strings.Contains(out, "wg-demux") {
		t.Errorf("the header must not describe a shared pod")
	}
}

// Nothing sets the mode in production, so the default must be the unprivileged one.
func TestWGDemuxDefaultIsUserspace(t *testing.T) {
	ds, c := wgDemux(t)
	if ds.Spec.Template.Spec.HostNetwork || envOf(c)["DEMUX_MODE"].Value != "userspace" || len(c.SecurityContext.Capabilities.Add) != 0 {
		t.Errorf("default: hostNetwork %v, mode %q, caps %+v", ds.Spec.Template.Spec.HostNetwork, envOf(c)["DEMUX_MODE"].Value, c.SecurityContext.Capabilities)
	}
	if lvl := wgNamespaceLevel(t); lvl != "restricted" {
		t.Errorf("the default namespace level: %q", lvl)
	}
	out, err := helmTemplate(t, "-s", "templates/operator/configmap.yaml")
	if err != nil || !strings.Contains(out, `WG_DEMUX_MODE: "userspace"`) {
		t.Fatalf("%v\n%s", err, out)
	}
}
