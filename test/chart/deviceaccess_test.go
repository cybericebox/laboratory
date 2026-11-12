package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

var consoleOn = append(append([]string{}, agentSet...), "--set", "terminal.enabled=true")

func deviceAccessDaemonSet(t *testing.T, extra ...string) appsv1.DaemonSet {
	t.Helper()
	var ds appsv1.DaemonSet
	render(t, "templates/deviceaccess/daemonset.yaml", &ds, append(append([]string{}, consoleOn...), extra...)...)
	return ds
}

// With the option off (the default) nothing of the device access service is rendered, the proxy has no console setting and no
// extra right.
func TestDeviceAccessIsNotRenderedWhileTheOptionIsOff(t *testing.T) {
	out, err := helmTemplate(t, agentSet...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, bad := range []string{"laboratory-device-access", "device-access", "TERMINAL_SERVICE_NAMESPACE", "laboratory-proxy-device-access"} {
		if strings.Contains(out, bad) {
			t.Errorf("terminal.enabled=false, but the chart renders %q", bad)
		}
	}
	// the proxy may not read devices without the console
	var role string
	for _, d := range strings.Split(out, "\n---\n") {
		if strings.Contains(d, "name: laboratory-proxy\n") && strings.Contains(d, "kind: ClusterRole\n") {
			role = d
		}
	}
	if role == "" || strings.Contains(role, "devices") {
		t.Errorf("the proxy ClusterRole without the console: %q", role)
	}
}

// The service is a DaemonSet of its own, on the pod network, with nothing but the runtime socket and the FIFO directory of the node.
func TestDeviceAccessDaemonSetIsLeanAndHasNoAPIRights(t *testing.T) {
	ds := deviceAccessDaemonSet(t)
	spec := ds.Spec.Template.Spec
	if ds.Name != "laboratory-device-access" || spec.ServiceAccountName != "laboratory-device-access" {
		t.Fatalf("names %q %q", ds.Name, spec.ServiceAccountName)
	}
	if spec.HostNetwork || spec.HostPID || spec.HostIPC {
		t.Errorf("no host namespace: %v %v %v", spec.HostNetwork, spec.HostPID, spec.HostIPC)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Errorf("the pod must not mount a service account token")
	}
	if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil || string(spec.SecurityContext.SeccompProfile.Type) != "RuntimeDefault" {
		t.Errorf("seccomp RuntimeDefault expected: %+v", spec.SecurityContext)
	}
	if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 {
		t.Fatalf("one container: %d, %d", len(spec.Containers), len(spec.InitContainers))
	}
	c := spec.Containers[0]
	if strings.Join(c.Command, " ") != "/node device-access" || !strings.Contains(c.Image, "laboratory-node") {
		t.Errorf("command %v image %s", c.Command, c.Image)
	}
	sc := c.SecurityContext
	if sc == nil || sc.Privileged == nil || *sc.Privileged || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("security context: %+v", sc)
	}
	// Root without capabilities by default (the socket is root:root 0660); it is a value.
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Errorf("default user: %v", sc.RunAsUser)
	}
	// The socket and the FIFO directory sit at the same path in the pod as on the host.
	host := map[string]string{}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			host[v.Name] = v.HostPath.Path
		}
	}
	if len(host) != 2 {
		t.Fatalf("host paths: %v", host)
	}
	for _, m := range c.VolumeMounts {
		if p, ok := host[m.Name]; ok && p != m.MountPath {
			t.Errorf("%s is mounted at %s, but is %s on the host", m.Name, m.MountPath, p)
		}
	}
	if host["runtime-sock"] != "/run/k0s/containerd.sock" || host["fifo"] != "/run/containerd/fifo" {
		t.Errorf("paths: %v", host)
	}
	// Exec probes only: no port is open to the kubelet.
	for name, p := range map[string]*corev1.Probe{"startup": c.StartupProbe, "readiness": c.ReadinessProbe, "liveness": c.LivenessProbe} {
		if p == nil || p.Exec == nil {
			t.Errorf("%s probe must be an exec probe: %+v", name, p)
		}
	}
}

func TestDeviceAccessPathsAndNonRootAreValues(t *testing.T) {
	ds := deviceAccessDaemonSet(t, "--set", "terminal.containerdSocket=/run/containerd/containerd.sock", "--set", "terminal.fifoDir=/run/lab-exec",
		"--set", "terminal.securityContext.runAsUser=65532", "--set", "terminal.securityContext.runAsGroup=65532", "--set", "terminal.securityContext.supplementalGroups={998}")
	spec := ds.Spec.Template.Spec
	paths := map[string]string{}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			paths[v.Name] = v.HostPath.Path
		}
	}
	if paths["runtime-sock"] != "/run/containerd/containerd.sock" || paths["fifo"] != "/run/lab-exec" {
		t.Errorf("paths %v", paths)
	}
	sc := spec.Containers[0].SecurityContext
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || *sc.RunAsUser != 65532 || len(spec.SecurityContext.SupplementalGroups) != 1 {
		t.Errorf("non-root: %+v %+v", sc, spec.SecurityContext)
	}
}

// No Role, ClusterRole or binding names the service's account: it has no Kubernetes API rights.
func TestDeviceAccessAccountHasNoRights(t *testing.T) {
	out, err := helmTemplate(t, consoleOn...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, d := range docs(t, out) {
		kind, _ := d["kind"].(string)
		if kind != "RoleBinding" && kind != "ClusterRoleBinding" {
			continue
		}
		raw, _ := yaml.Marshal(d["subjects"])
		if strings.Contains(string(raw), "laboratory-device-access") {
			t.Errorf("%s %v binds the service account", kind, d["metadata"])
		}
	}
}

// The listener trusts its caller: ingress only from the L7 proxy pods on one port, and no egress.
func TestDeviceAccessPolicyAdmitsOnlyTheProxy(t *testing.T) {
	out, err := helmTemplate(t, append(append([]string{}, consoleOn...), "-s", "templates/deviceaccess/netpol.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var policy struct {
		Spec struct {
			EndpointSelector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"endpointSelector"`
			Ingress []struct {
				FromEndpoints []struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"fromEndpoints"`
				ToPorts []struct {
					Ports []struct{ Port, Protocol string } `json:"ports"`
				} `json:"toPorts"`
			} `json:"ingress"`
			Egress []map[string]any `json:"egress"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &policy); err != nil {
		t.Fatal(err)
	}
	s := policy.Spec
	if s.EndpointSelector.MatchLabels["app"] != "laboratory-device-access" {
		t.Errorf("selector %v", s.EndpointSelector)
	}
	if len(s.Ingress) != 1 || len(s.Ingress[0].FromEndpoints) != 1 {
		t.Fatalf("ingress %+v", s.Ingress)
	}
	from := s.Ingress[0].FromEndpoints[0].MatchLabels
	if from["app"] != "laboratory-proxy-l7" || from["k8s:io.kubernetes.pod.namespace"] != "laboratory-proxy" || len(from) != 2 {
		t.Errorf("from %v", from)
	}
	if ports := s.Ingress[0].ToPorts; len(ports) != 1 || len(ports[0].Ports) != 1 || ports[0].Ports[0].Port != "8080" || ports[0].Ports[0].Protocol != "TCP" {
		t.Errorf("ports %+v", ports)
	}
	if len(s.Egress) != 1 || len(s.Egress[0]) != 0 {
		t.Errorf("egress must be one empty rule (deny all): %v", s.Egress)
	}
}

func TestConsoleIsRefusedWithoutItsPolicies(t *testing.T) {
	for _, tc := range []struct {
		set  []string
		want string
	}{
		{[]string{"--set", "terminal.networkPolicy.enabled=false"}, "terminal.networkPolicy.enabled=false"},
		{[]string{"--set", "proxy.networkPolicy.enabled=false"}, "proxy.networkPolicy.enabled=false"},
		{[]string{"--set", "proxy.l7.enabled=false"}, "needs the L7 proxy"},
		{[]string{"--set", "terminal.port=9102"}, "two different ports"},
		{[]string{"--set", "terminal.fifoDir=relative/dir"}, "absolute paths"},
		{[]string{"--set", "terminal.limits.maxSessionBytes=1"}, "maxSessionBytes"},
	} {
		out, err := helmTemplate(t, append(append([]string{}, consoleOn...), tc.set...)...)
		if err == nil || !strings.Contains(out+err.Error(), tc.want) {
			t.Errorf("%v must be refused (%q): %v\n%s", tc.set, tc.want, err, out)
		}
	}
	// the policies may be switched off while the console is off
	if out, err := helmTemplate(t, append(append([]string{}, agentSet...), "--set", "terminal.networkPolicy.enabled=false")...); err != nil {
		t.Errorf("console off, its policy off: %v\n%s", err, out)
	}
}

// The proxy reaches the device pods of the lab namespaces (not every pod there), and the service only with the console on.
func TestProxyEgressIsNarrowedToDevicePods(t *testing.T) {
	for _, on := range []bool{false, true} {
		set := agentSet
		if on {
			set = consoleOn
		}
		out, err := helmTemplate(t, append(append([]string{}, set...), "-s", "templates/netpol/proxy-l7.yaml")...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		cnp := out[strings.Index(out, "kind: CiliumNetworkPolicy"):]
		if !strings.Contains(cnp, "k8s:laboratory.cybericebox.com/device") || !strings.Contains(cnp, "k8s:io.cilium.k8s.namespace.labels.laboratory.cybericebox.com/group") {
			t.Errorf("console %v: the lab egress is not limited to device pods:\n%s", on, cnp)
		}
		if got := strings.Contains(cnp, "app: laboratory-device-access"); got != on {
			t.Errorf("console %v: the rule for the service is present: %v", on, got)
		}
		if strings.Count(cnp, "toEndpoints") != 1+map[bool]int{false: 0, true: 1}[on] {
			t.Errorf("console %v: unexpected number of egress endpoint rules:\n%s", on, cnp)
		}
	}
}

// The proxy gets the two read rights the console needs, narrow: devices (get, list, watch) and the EndpointSlices of the service's own namespace.
// No pod is readable by it through the console.
func TestProxyConsoleRBAC(t *testing.T) {
	out, err := helmTemplate(t, consoleOn...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var sawRole, sawDevices bool
	for _, d := range docs(t, out) {
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		kind, _ := d["kind"].(string)
		raw, _ := yaml.Marshal(d["rules"])
		switch {
		case kind == "Role" && name == "laboratory-proxy-device-access":
			sawRole = true
			if meta["namespace"] != "laboratory-device-access" || !strings.Contains(string(raw), "endpointslices") || strings.Contains(string(raw), "pods") ||
				strings.Contains(string(raw), "secrets") || strings.Contains(string(raw), "create") || strings.Contains(string(raw), "update") || strings.Contains(string(raw), "delete") {
				t.Errorf("role: %v %s", meta, raw)
			}
		case kind == "ClusterRole" && name == "laboratory-proxy":
			sawDevices = strings.Contains(string(raw), "devices")
			if strings.Contains(string(raw), "pods") || strings.Contains(string(raw), "endpointslices") {
				t.Errorf("the proxy may not read pods or slices cluster-wide: %s", raw)
			}
		case kind == "Role" && strings.Contains(string(raw), "pods") && strings.Contains(name, "proxy"):
			t.Errorf("a Role that lets the proxy read pods: %s", name)
		}
	}
	if !sawRole || !sawDevices {
		t.Errorf("role %v, devices %v", sawRole, sawDevices)
	}
}

// The service has a namespace of its own (privileged for the hostPath), a headless Service for its slices, and nothing else of the platform in it.
func TestDeviceAccessHasANamespaceOfItsOwn(t *testing.T) {
	out, err := helmTemplate(t, consoleOn...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	inNS := map[string]bool{}
	var sawNS, sawSvc bool
	for _, d := range docs(t, out) {
		meta, _ := d["metadata"].(map[string]any)
		kind, _ := d["kind"].(string)
		if meta["namespace"] == "laboratory-device-access" {
			inNS[kind+"/"+meta["name"].(string)] = true
		}
		if kind == "Namespace" && meta["name"] == "laboratory-device-access" {
			sawNS = true
			if l, _ := meta["labels"].(map[string]any); l["pod-security.kubernetes.io/enforce"] != "privileged" {
				t.Errorf("namespace labels: %v", l)
			}
		}
		if kind == "Service" && meta["name"] == "laboratory-device-access" {
			sawSvc = true
			spec, _ := d["spec"].(map[string]any)
			if spec["clusterIP"] != "None" {
				t.Errorf("the service must be headless: %v", spec["clusterIP"])
			}
		}
	}
	if !sawNS || !sawSvc {
		t.Fatalf("namespace %v, service %v", sawNS, sawSvc)
	}
	want := map[string]bool{"ServiceAccount/laboratory-device-access": true, "DaemonSet/laboratory-device-access": true, "Service/laboratory-device-access": true,
		"CiliumNetworkPolicy/laboratory-device-access": true, "Secret/laboratory-device-access-auth": true, "Role/laboratory-proxy-device-access": true,
		"RoleBinding/laboratory-proxy-device-access": true}
	for k := range inNS {
		if !want[k] {
			t.Errorf("unexpected object in the service's namespace: %s", k)
		}
	}
	for k := range want {
		if !inNS[k] {
			t.Errorf("missing in the service's namespace: %s", k)
		}
	}
	// it must be a namespace of its own
	if out, err := helmTemplate(t, append(append([]string{}, consoleOn...), "--set", "terminal.namespace=laboratory-system")...); err == nil {
		t.Errorf("the release namespace was accepted:\n%s", out)
	}
	if out, err := helmTemplate(t, append(append([]string{}, consoleOn...), "--set", "terminal.namespace=laboratory-proxy")...); err == nil {
		t.Errorf("the proxy namespace was accepted:\n%s", out)
	}
}

func TestDeviceAccessListensOnThePodIPOnly(t *testing.T) {
	ds := deviceAccessDaemonSet(t)
	env := map[string]corev1.EnvVar{}
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	if env["POD_IP"].ValueFrom == nil || env["POD_IP"].ValueFrom.FieldRef.FieldPath != "status.podIP" {
		t.Errorf("POD_IP: %+v", env["POD_IP"])
	}
	for _, k := range []string{"LISTEN", "METRICS_LISTEN"} {
		if !strings.HasPrefix(env[k].Value, "$(POD_IP):") {
			t.Errorf("%s = %q: bound to every interface", k, env[k].Value)
		}
	}
	// POD_IP is declared before the variables that use it (the kubelet expands $(VAR) only from earlier ones)
	order := map[string]int{}
	for i, e := range ds.Spec.Template.Spec.Containers[0].Env {
		order[e.Name] = i
	}
	if order["POD_IP"] > order["LISTEN"] || order["POD_IP"] > order["METRICS_LISTEN"] {
		t.Error("POD_IP must come before LISTEN")
	}
}

// The shared secret: one value in two Secrets, mounted as files, never an environment variable; the chart refuses a short one.
func TestSharedSecretIsMountedAsFilesAndNeverInTheEnvironment(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef0123456789"
	set := append(append([]string{}, consoleOn...), "--set", "terminal.sharedSecret.value="+secret, "--set", "terminal.sharedSecret.accept=fedcba9876543210fedcba9876543210fedcba98")
	out, err := helmTemplate(t, set...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	values := map[string]map[string]any{}
	for _, d := range docs(t, out) {
		meta, _ := d["metadata"].(map[string]any)
		if d["kind"] == "Secret" && strings.HasSuffix(meta["name"].(string), "device-access-auth") {
			values[meta["namespace"].(string)], _ = d["stringData"].(map[string]any)
		}
	}
	if values["laboratory-device-access"]["current"] != secret || values["laboratory-proxy"]["current"] != secret {
		t.Fatalf("secrets: %v", values)
	}
	if values["laboratory-device-access"]["accept"] == nil || values["laboratory-proxy"]["accept"] != nil {
		t.Errorf("the service accepts the second secret, the proxy never signs with it: %v", values)
	}
	// nowhere in a workload: no env value, no argument
	for _, d := range docs(t, out) {
		kind, _ := d["kind"].(string)
		if kind != "DaemonSet" && kind != "Deployment" {
			continue
		}
		raw, _ := yaml.Marshal(d)
		if strings.Contains(string(raw), secret) {
			t.Errorf("the secret appears in a %s", kind)
		}
	}
	ds := deviceAccessDaemonSet(t)
	var mounted bool
	for _, m := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "auth" && m.MountPath == "/etc/device-access/auth" && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Error("the service does not mount the secret read-only")
	}
	var dep appsv1.Deployment
	render(t, "templates/proxy/deployment.yaml", &dep, consoleOn...)
	var proxyMount bool
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		proxyMount = proxyMount || m.Name == "device-access-auth" && m.MountPath == "/etc/proxy/device-access-auth" && m.ReadOnly
	}
	if !proxyMount {
		t.Error("the proxy does not mount the secret")
	}
	for _, short := range []string{"terminal.sharedSecret.value=short", "terminal.sharedSecret.accept=short"} {
		if out, err := helmTemplate(t, append(append([]string{}, consoleOn...), "--set", short)...); err == nil {
			t.Errorf("%s accepted:\n%s", short, out)
		}
	}
	// without a value the chart makes one of at least 32 characters, the same in both namespaces
	out, err = helmTemplate(t, consoleOn...)
	if err != nil {
		t.Fatal(err)
	}
	gen := map[string]string{}
	for _, d := range docs(t, out) {
		meta, _ := d["metadata"].(map[string]any)
		if d["kind"] == "Secret" && strings.HasSuffix(meta["name"].(string), "device-access-auth") {
			sd, _ := d["stringData"].(map[string]any)
			gen[meta["namespace"].(string)], _ = sd["current"].(string)
		}
	}
	if len(gen["laboratory-proxy"]) < 32 || gen["laboratory-proxy"] != gen["laboratory-device-access"] {
		t.Errorf("generated secrets: %v", gen)
	}
}

func TestEmbeddingListsAreValidatedAndReachTheProxy(t *testing.T) {
	var dep appsv1.Deployment
	render(t, "templates/proxy/deployment.yaml", &dep, append(append([]string{}, consoleOn...),
		"--set", "terminal.embedding.allow={https://exercises.example.com,https://*.example.org}", "--set", "terminal.embedding.deny={https://vpn.example.org}")...)
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["TERMINAL_EMBED_ALLOW"] != "https://exercises.example.com,https://*.example.org" || env["TERMINAL_EMBED_DENY"] != "https://vpn.example.org" {
		t.Errorf("env: %v", env)
	}
	render(t, "templates/proxy/deployment.yaml", &dep, consoleOn...)
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if (e.Name == "TERMINAL_EMBED_ALLOW" || e.Name == "TERMINAL_EMBED_DENY") && e.Value != "" {
			t.Errorf("%s = %q by default: no embedding is the default", e.Name, e.Value)
		}
	}
	for _, bad := range []string{"http://a.example.com", "https://*.com", "https://**.example.com", "https://a.example.com/x", "https://A.example.com", "*.example.com",
		"https://a.*.example.com", "https://a.example.com:port", "https://*"} {
		for _, list := range []string{"allow", "deny"} {
			if out, err := helmTemplate(t, append(append([]string{}, consoleOn...), "--set", "terminal.embedding."+list+"={"+bad+"}")...); err == nil {
				t.Errorf("%s entry %q accepted:\n%s", list, bad, out)
			}
		}
	}
}

func TestProxyGetsTheConsoleSettings(t *testing.T) {
	var dep appsv1.Deployment
	render(t, "templates/proxy/deployment.yaml", &dep, append(append([]string{}, consoleOn...), "--set", "terminal.limits.perClient=5")...)
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for k, want := range map[string]string{"TERMINAL_ENABLED": "true", "TERMINAL_SERVICE_NAMESPACE": "laboratory-device-access", "TERMINAL_SERVICE_PORT": "8080",
		"TERMINAL_PER_CLIENT": "5", "TERMINAL_MAX_FILE_BYTES": "16777216", "TERMINAL_MAX_SESSION_BYTES": "67108864"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
}

// The service is rendered for the checker alone (no console): without a way for the network in, and with the checker socket.
func TestDeviceAccessServesTheCheckerWithoutTheConsole(t *testing.T) {
	var ds appsv1.DaemonSet
	render(t, "templates/deviceaccess/daemonset.yaml", &ds, append(append([]string{}, agentSet...), "--set", "checks.enabled=true")...)
	spec := ds.Spec.Template.Spec
	env := map[string]string{}
	for _, e := range spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["CHECKER_SOCKET"] != "/run/cib-checker/checker.sock" || env["CHECKER_UID"] != "65532" || env["CHECKER_GID"] != "65532" || env["CHECKER_MAX_SECONDS"] != "120" {
		t.Errorf("env %v", env)
	}
	if len(spec.SecurityContext.SupplementalGroups) != 1 || spec.SecurityContext.SupplementalGroups[0] != 65532 {
		t.Errorf("the service must be a member of the checker's group (to hand it the socket without CAP_CHOWN): %v", spec.SecurityContext.SupplementalGroups)
	}
	var sock string
	for _, v := range spec.Volumes {
		if v.Name == "checker-socket" && v.HostPath != nil {
			sock = v.HostPath.Path
		}
	}
	if sock != "/run/cib-checker" {
		t.Errorf("the socket directory: %q", sock)
	}
	// with the console off: no proxy secret, no ingress, no Role for the proxy
	out, err := helmTemplate(t, append(append([]string{}, agentSet...), "--set", "checks.enabled=true")...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "laboratory-proxy-device-access") || strings.Contains(out, "TERMINAL_SERVICE_NAMESPACE") {
		t.Error("the console's pieces are rendered with the console off")
	}
	pol, err := helmTemplate(t, append(append([]string{}, agentSet...), "--set", "checks.enabled=true", "-s", "templates/deviceaccess/netpol.yaml")...)
	if err != nil || strings.Contains(pol, "fromEndpoints") || !strings.Contains(pol, "ingress:") {
		t.Errorf("the policy of a service used by the checker alone admits nobody over the network: %v\n%s", err, pol)
	}
	// without the checker the socket is not there
	ds2 := deviceAccessDaemonSet(t)
	for _, e := range ds2.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "CHECKER_SOCKET" {
			t.Error("a checker socket without the checker")
		}
	}
}

// The checker is refused without its policies. No setting names a console host any more (the host is per device).
func TestCheckerRefusals(t *testing.T) {
	on := append(append([]string{}, agentSet...), "--set", "checks.enabled=true")
	for _, tc := range []struct{ set, want string }{
		{"checks.networkPolicy.enabled=false", "checks.networkPolicy.enabled=false"},
		{"operator.admissionPolicy.enabled=false", "operator.admissionPolicy.enabled=false"},
		{"terminal.networkPolicy.enabled=false", "device access service"},
		{"checks.maxTimeLimitSeconds=601", "maxTimeLimitSeconds"},
		{"checks.maxTimeLimitSeconds=0", "maxTimeLimitSeconds"},
		{"checks.socketUid=0", "not root"},
		{"checks.socketGid=0", "not root"},
		{"checks.socketDir=run/x", "absolute"},
	} {
		out, err := helmTemplate(t, append(append([]string{}, on...), "--set", tc.set)...)
		if err == nil || !strings.Contains(out+err.Error(), tc.want) {
			t.Errorf("%s must be refused (%q): %v\n%s", tc.set, tc.want, err, out)
		}
	}
	// terminal.host is gone: the console host is made per device, and a value for it is a key the chart does not read
	if out, err := helmTemplate(t, append(append([]string{}, on...), "--set", "terminal.host=shell")...); err != nil || strings.Contains(out, "TERMINAL_HOST") {
		t.Errorf("a removed value changed the render: %v", err)
	}
	// the policies may be off while the checker is off
	if out, err := helmTemplate(t, append(append([]string{}, agentSet...), "--set", "checks.networkPolicy.enabled=false")...); err != nil {
		t.Errorf("checker off, its policy off: %v\n%s", err, out)
	}
}

func TestCheckerSecretsRoleAndTheOperatorsBinding(t *testing.T) {
	var found bool
	for _, d := range decodeDocs(t, "templates/checker/rbac.yaml", checksOn...) {
		if strings.Contains(d, "name: laboratory-checker-secrets\n") && strings.Contains(d, "kind: ClusterRole\n") {
			found = true
			var role rbacv1.ClusterRole
			if err := yaml.Unmarshal([]byte(d), &role); err != nil {
				t.Fatal(err)
			}
			if len(role.Rules) != 1 || strings.Join(role.Rules[0].Resources, ",") != "secrets" || strings.Join(role.Rules[0].Verbs, ",") != "get" {
				t.Errorf("the secrets role: %+v", role.Rules)
			}
		}
		// no binding of it in the chart: the operator makes one per group namespace
		if strings.Contains(d, "kind: ClusterRoleBinding") && strings.Contains(d, "laboratory-checker-secrets") {
			t.Error("the secrets role is bound cluster-wide")
		}
	}
	if !found {
		t.Error("no ClusterRole laboratory-checker-secrets")
	}
}
