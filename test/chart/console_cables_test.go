package chart_test

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// consoleCables.enabled is off by default and reaches the operator and the agent through the shared environment.
func TestConsoleCablesSwitchReachesTheComponents(t *testing.T) {
	if env := operatorConfig(t); env["CONSOLE_CABLES_ENABLED"] != "false" {
		t.Errorf("default CONSOLE_CABLES_ENABLED = %q, want false", env["CONSOLE_CABLES_ENABLED"])
	}
	if env := operatorConfig(t, "--set", "consoleCables.enabled=true"); env["CONSOLE_CABLES_ENABLED"] != "true" {
		t.Errorf("CONSOLE_CABLES_ENABLED = %q, want true", env["CONSOLE_CABLES_ENABLED"])
	}
}

// The lines are moved by the device access service, so consoleCables.enabled renders it even with the console and the checks off; with
// all three off it is not rendered.
func TestConsoleCablesRenderTheDeviceAccessService(t *testing.T) {
	out, err := helmTemplate(t, append(append([]string{}, agentSet...), "--set", "consoleCables.enabled=true", "-s", "templates/deviceaccess/daemonset.yaml")...)
	if err != nil || !strings.Contains(out, "kind: DaemonSet") {
		t.Fatalf("consoleCables.enabled must render the device access DaemonSet: %v\n%s", err, out)
	}
	if out, err := helmTemplate(t, append(append([]string{}, agentSet...), "-s", "templates/deviceaccess/daemonset.yaml")...); err == nil && strings.Contains(out, "kind: DaemonSet") {
		t.Fatal("with every user off the service is not rendered")
	}
}

func withConsolePort(s *laboratoryv1alpha1.LabSpec) {
	s.Devices = append(s.Devices, laboratoryv1alpha1.DeviceTemplate{Name: "r1", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "frr",
		ConsolePorts: []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0"}}})
}

func withConsoleCable(s *laboratoryv1alpha1.LabSpec) {
	s.ConsoleCables = append(s.ConsoleCables, laboratoryv1alpha1.ConsoleCableTemplate{
		Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}})
}

// With consoleCables.enabled off the API server refuses a lab with a port or a cable (CONSOLE_CABLES_UNAVAILABLE); with it on they pass.
func TestFeatureAdmissionConsoleCables(t *testing.T) {
	ctx := context.Background()
	f := startFeatureCluster(t, nil)
	ns := f.group(t, "cc-off", nil, nil)
	refused(t, f.other.Create(ctx, lab(ns, "port", withConsolePort)), "CONSOLE_CABLES_UNAVAILABLE")
	refused(t, f.other.Create(ctx, lab(ns, "cable", withConsoleCable)), "CONSOLE_CABLES_UNAVAILABLE")
	accepted(t, f.other.Create(ctx, lab(ns, "plain", nil)))
	// a Device or a ConsoleCable made directly (not through a Lab) is refused too
	dev := func(name string, ports bool) *laboratoryv1alpha1.Device {
		d := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: laboratoryv1alpha1.DeviceSpec{LabRef: "l", Name: name, Type: laboratoryv1alpha1.DeviceTypeContainer}}
		if ports {
			d.Spec.SerialPorts = []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}}
		}
		return d
	}
	refused(t, f.other.Create(ctx, dev("pc1", true)), "CONSOLE_CABLES_UNAVAILABLE")
	accepted(t, f.other.Create(ctx, dev("pc2", false)))
	cc := &laboratoryv1alpha1.ConsoleCable{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: ns}, Spec: laboratoryv1alpha1.ConsoleCableSpec{LabRef: "l",
		Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}}}
	refused(t, f.other.Create(ctx, cc), "CONSOLE_CABLES_UNAVAILABLE")

	on := startFeatureCluster(t, nil, "--set", "consoleCables.enabled=true")
	ns = on.group(t, "cc-on", nil, nil)
	accepted(t, on.other.Create(ctx, lab(ns, "port", withConsolePort)))
	accepted(t, on.other.Create(ctx, lab(ns, "cable", withConsoleCable)))
	accepted(t, on.other.Create(ctx, &laboratoryv1alpha1.ConsoleCable{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: ns}, Spec: laboratoryv1alpha1.ConsoleCableSpec{LabRef: "l",
		Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}}}))
}

var cablesOn = append(append([]string{}, agentSet...), "--set", "consoleCables.enabled=true")

// With console cables on, the device access service gets cable.sock (in the node directory it shares with the node-agent), the listener of
// the cable stream on its pod IP and the key of that stream, which no other component mounts.
func TestConsoleCablesDeviceAccess(t *testing.T) {
	var ds appsv1.DaemonSet
	render(t, "templates/deviceaccess/daemonset.yaml", &ds, cablesOn...)
	spec := ds.Spec.Template.Spec
	env := envOf(spec.Containers[0])
	want := map[string]string{
		"CABLE_SOCKET": "/run/cybericebox/console-cables/cable.sock", "CABLE_PEER_UID": "0", "CABLE_GID": "0",
		"CABLE_LISTEN": "$(POD_IP):8081", "CABLE_KEY_FILE": "/etc/device-access/cable-key/key",
	}
	for k, v := range want {
		if env[k].Value != v {
			t.Errorf("%s = %q, want %q", k, env[k].Value, v)
		}
	}
	port := false
	for _, p := range spec.Containers[0].Ports {
		port = port || p.Name == "cable" && p.ContainerPort == 8081
	}
	if !port {
		t.Error("the cable port is not declared")
	}
	var hostDir, secret string
	for _, v := range spec.Volumes {
		if v.Name == "cable-socket" && v.HostPath != nil {
			hostDir = v.HostPath.Path
		}
		if v.Name == "cable-key" && v.Secret != nil {
			secret = v.Secret.SecretName
		}
	}
	if hostDir != "/run/cybericebox/console-cables" || secret != "laboratory-console-cable-key" {
		t.Errorf("volumes: %q %q", hostDir, secret)
	}

	// off: nothing of it
	var off appsv1.DaemonSet
	render(t, "templates/deviceaccess/daemonset.yaml", &off, consoleOn...)
	for k := range envOf(off.Spec.Template.Spec.Containers[0]) {
		if strings.HasPrefix(k, "CABLE_") {
			t.Errorf("console cables off, but %s is set", k)
		}
	}
	if out, err := helmTemplate(t, append(append([]string{}, consoleOn...), "-s", "templates/deviceaccess/console-cable-key.yaml")...); err == nil && strings.Contains(out, "kind: Secret") {
		t.Error("console cables off, but the key is rendered")
	}
}

// The key is a Secret of the device access namespace, long and random, mounted by the device access DaemonSet only.
func TestConsoleCableKey(t *testing.T) {
	var s corev1.Secret
	render(t, "templates/deviceaccess/console-cable-key.yaml", &s, cablesOn...)
	if s.Namespace != "laboratory-device-access" || len(s.StringData["key"]) != 64 {
		t.Fatalf("secret %s/%s, key of %d bytes", s.Namespace, s.Name, len(s.StringData["key"]))
	}
	out, err := helmTemplate(t, cablesOn...)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		if strings.Contains(doc, "laboratory-console-cable-key") && !strings.Contains(doc, "name: laboratory-device-access\n") && !strings.Contains(doc, "kind: Secret") {
			t.Errorf("the key is referenced outside the device access DaemonSet:\n%s", doc)
		}
	}
}

type ciliumPeer struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type ciliumRule struct {
	FromEndpoints []ciliumPeer `json:"fromEndpoints"`
	ToEndpoints   []ciliumPeer `json:"toEndpoints"`
	ToPorts       []struct {
		Ports []struct {
			Port string `json:"port"`
		} `json:"ports"`
	} `json:"toPorts"`
}

type ciliumPolicy struct {
	Spec struct {
		Ingress []ciliumRule `json:"ingress"`
		Egress  []ciliumRule `json:"egress"`
	} `json:"spec"`
}

func rulePorts(r ciliumRule) string {
	var out []string
	for _, tp := range r.ToPorts {
		for _, p := range tp.Ports {
			out = append(out, p.Port)
		}
	}
	return strings.Join(out, ",")
}

// The Cilium policy of the service with console cables: the device access pods reach each other on the cable port and nothing else; the
// proxy still only on the stream port, and only with the console on.
func TestConsoleCablesNetworkPolicy(t *testing.T) {
	var p ciliumPolicy
	render(t, "templates/deviceaccess/netpol.yaml", &p, cablesOn...)
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].FromEndpoints) != 1 || p.Spec.Ingress[0].FromEndpoints[0].MatchLabels["app"] != "laboratory-device-access" ||
		p.Spec.Ingress[0].FromEndpoints[0].MatchLabels["k8s:io.kubernetes.pod.namespace"] != "laboratory-device-access" ||
		rulePorts(p.Spec.Ingress[0]) != "8081" {
		t.Errorf("ingress: %+v", p.Spec.Ingress)
	}
	if len(p.Spec.Egress) != 1 || len(p.Spec.Egress[0].ToEndpoints) != 1 || p.Spec.Egress[0].ToEndpoints[0].MatchLabels["app"] != "laboratory-device-access" ||
		rulePorts(p.Spec.Egress[0]) != "8081" {
		t.Errorf("egress: %+v", p.Spec.Egress)
	}
	var both ciliumPolicy
	render(t, "templates/deviceaccess/netpol.yaml", &both, append(append([]string{}, cablesOn...), "--set", "terminal.enabled=true")...)
	if len(both.Spec.Ingress) != 2 || both.Spec.Ingress[0].FromEndpoints[0].MatchLabels["app"] != "laboratory-proxy-l7" || rulePorts(both.Spec.Ingress[0]) != "8080" {
		t.Errorf("ingress with the console: %+v", both.Spec.Ingress)
	}
	var none ciliumPolicy
	render(t, "templates/deviceaccess/netpol.yaml", &none, consoleOn...)
	if len(none.Spec.Egress) != 1 || len(none.Spec.Egress[0].ToEndpoints) != 0 {
		t.Errorf("without cables there is no egress: %+v", none.Spec.Egress)
	}
}
