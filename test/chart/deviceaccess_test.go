package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	for _, bad := range []string{"laboratory-device-access", "device-access", "TERMINAL_HOST", "TERMINAL_SERVICE_NAMESPACE", "laboratory-proxy-device-access"} {
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
		{[]string{"--set", "terminal.host=Not_A_Label"}, "not a DNS label"},
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

// The proxy gets the two read rights the console needs, narrow: devices (get, list, watch) and pods of the release namespace only.
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
			if meta["namespace"] != "laboratory-system" || !strings.Contains(string(raw), "pods") || strings.Contains(string(raw), "create") ||
				strings.Contains(string(raw), "update") || strings.Contains(string(raw), "delete") || strings.Contains(string(raw), "exec") {
				t.Errorf("role: %v %s", meta, raw)
			}
		case kind == "ClusterRole" && name == "laboratory-proxy":
			sawDevices = strings.Contains(string(raw), "devices")
			if strings.Contains(string(raw), "pods") {
				t.Errorf("the proxy may not read pods cluster-wide: %s", raw)
			}
		}
	}
	if !sawRole || !sawDevices {
		t.Errorf("role %v, devices %v", sawRole, sawDevices)
	}
}

func TestProxyGetsTheConsoleSettings(t *testing.T) {
	var dep appsv1.Deployment
	render(t, "templates/proxy/deployment.yaml", &dep, append(append([]string{}, consoleOn...), "--set", "terminal.limits.perClient=5", "--set", "terminal.host=shell")...)
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for k, want := range map[string]string{"TERMINAL_ENABLED": "true", "TERMINAL_HOST": "shell", "TERMINAL_SERVICE_NAMESPACE": "laboratory-system", "TERMINAL_SERVICE_PORT": "8080",
		"TERMINAL_PER_CLIENT": "5", "TERMINAL_MAX_FILE_BYTES": "16777216", "TERMINAL_MAX_SESSION_BYTES": "67108864"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
}
