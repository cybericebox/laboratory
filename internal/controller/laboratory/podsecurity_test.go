package laboratory

import (
	"os"
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/profiles"
)

func newDeviceForPod() *laboratoryv1alpha1.Device {
	d := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: "lab-web", Namespace: "ns"}}
	d.Spec = laboratoryv1alpha1.DeviceSpec{Name: "web", LabRef: "lab", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx"}
	return d
}

// A device pod gets no token, the runtime's seccomp profile, no service links, dropped capabilities and a storage limit.
func TestDevicePodIsHardened(t *testing.T) {
	r := &DeviceReconciler{Security: PodSecurity{EphemeralStorage: "2Gi"}}
	_, _, _, spec := r.workloadTemplate(newDeviceForPod(), false)
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("a device must not get the service account token")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Error("no service links")
	}
	if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccomp = %+v", spec.SecurityContext)
	}
	if spec.HostUsers != nil {
		t.Error("user namespaces are off in this reconciler unless asked for")
	}
	if len(spec.SecurityContext.Sysctls) != 1 || spec.SecurityContext.Sysctls[0].Name != "net.ipv4.ping_group_range" || spec.SecurityContext.Sysctls[0].Value != "0 65535" {
		t.Errorf("every device pings through ping_group_range: %+v", spec.SecurityContext.Sysctls)
	}
	if _, ok := spec.Containers[0].Resources.Limits["cybericebox.com/tun"]; ok {
		t.Error("a standard device gets no tun")
	}
	c := spec.Containers[0]
	if c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("caps: %+v", c.SecurityContext.Capabilities)
	}
	want := resource.MustParse("2Gi")
	if got := c.Resources.Limits[corev1.ResourceEphemeralStorage]; got.Cmp(want) != 0 {
		t.Errorf("ephemeral-storage limit = %v", got.String())
	}
	if got := c.Resources.Requests[corev1.ResourceEphemeralStorage]; got.Cmp(want) != 0 {
		t.Errorf("ephemeral-storage request = %v", got.String())
	}
}

func TestDevicePodUserNamespacesWhenAsked(t *testing.T) {
	r := &DeviceReconciler{Security: PodSecurity{UserNamespaces: true}}
	_, _, _, spec := r.workloadTemplate(newDeviceForPod(), false)
	if spec.HostUsers == nil || *spec.HostUsers {
		t.Fatal("hostUsers must be false")
	}
	if _, ok := spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage]; ok {
		t.Error("no ephemeral-storage limit configured: none set")
	}
}

func TestNetconfigInitContainerKeepsOnlyItsCapabilities(t *testing.T) {
	d := newDeviceForPod()
	d.Spec.Interfaces = []laboratoryv1alpha1.InterfaceSpec{{Name: "eth1", Addr: laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic, IP: "10.0.0.2/24"}}}
	r := &DeviceReconciler{NetConfigImage: "node"}
	_, _, _, spec := r.workloadTemplate(d, false)
	if len(spec.InitContainers) != 1 {
		t.Fatalf("init containers = %d", len(spec.InitContainers))
	}
	sc := spec.InitContainers[0].SecurityContext
	if sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 2 || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("netconfig security context: %+v", sc)
	}
}

func oldVPNSpec() corev1.PodSpec {
	priv := true
	return corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "conntrack-accounting", SecurityContext: &corev1.SecurityContext{Privileged: &priv}}},
		Containers: []corev1.Container{{Name: "vpn", SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"}}}}},
	}
}

// An old VPN pod is brought to the hardened shape (once), without the privileged init container.
func TestHardenGroupPodConvergesAnOldVPNPod(t *testing.T) {
	spec := oldVPNSpec()
	if !hardenGroupPod(&spec, "vpn", vpnCaps) {
		t.Fatal("an old pod must change")
	}
	if len(spec.InitContainers) != 0 {
		t.Errorf("no init container left: %+v", spec.InitContainers)
	}
	sc := spec.Containers[0].SecurityContext
	if sc.Privileged != nil || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 2 || *sc.AllowPrivilegeEscalation {
		t.Errorf("vpn container: %+v", sc)
	}
	if spec.AutomountServiceAccountToken == nil || !*spec.AutomountServiceAccountToken {
		t.Error("the VPN pod talks to the API server and keeps its token")
	}
	if spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccomp")
	}
	if hardenGroupPod(&spec, "vpn", vpnCaps) {
		t.Error("a hardened pod is left alone (no restart loop)")
	}
}

func TestGatewayKeepsTheCapabilitiesItNeeds(t *testing.T) {
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "gateway"}}}
	hardenGroupPod(&spec, "gateway", gatewayCaps)
	got := map[corev1.Capability]bool{}
	for _, c := range spec.Containers[0].SecurityContext.Capabilities.Add {
		got[c] = true
	}
	if len(got) != 3 || !got["NET_ADMIN"] || !got["NET_RAW"] || !got["NET_BIND_SERVICE"] {
		t.Errorf("gateway caps = %v", got)
	}
}

// The extended profile requests the tun device from the node-agent's device plugin, request and limit.
func TestExtendedDeviceRequestsTun(t *testing.T) {
	for _, preset := range []laboratoryv1alpha1.SecurityPreset{laboratoryv1alpha1.SecurityPresetExtended, laboratoryv1alpha1.SecurityPresetNet, laboratoryv1alpha1.SecurityPresetDebug} {
		d := newDeviceForPod()
		d.Spec.SecurityPreset = preset
		r := &DeviceReconciler{}
		_, _, _, spec := r.workloadTemplate(d, false)
		res := spec.Containers[0].Resources
		one := resource.MustParse("1")
		if got := res.Limits["cybericebox.com/tun"]; got.Cmp(one) != 0 {
			t.Errorf("%s: tun limit = %v", preset, got.String())
		}
		if got := res.Requests["cybericebox.com/tun"]; got.Cmp(one) != 0 {
			t.Errorf("%s: tun request = %v", preset, got.String())
		}
	}
}

// With and without a user namespace the device pod carries the same ping range, and it fits the namespace's mapping.
func TestPingSysctlIsTheSameWithAndWithoutUserNamespaces(t *testing.T) {
	for _, userns := range []bool{false, true} {
		r := &DeviceReconciler{Security: PodSecurity{UserNamespaces: userns}}
		_, _, _, spec := r.workloadTemplate(newDeviceForPod(), false)
		sc := spec.SecurityContext.Sysctls
		if len(sc) != 1 || sc[0].Name != "net.ipv4.ping_group_range" || sc[0].Value != "0 65535" {
			t.Errorf("userns=%v: sysctls %+v", userns, sc)
		}
		if userns != (spec.HostUsers != nil && !*spec.HostUsers) {
			t.Errorf("userns=%v: hostUsers %v", userns, spec.HostUsers)
		}
	}
}

// The admission policy of the chart allows exactly the capabilities the operator can add: one missing would stop pods from
// being created, one extra would be a capability no profile may have.
func TestAdmissionPolicyCapabilitiesAreWhatTheCodeAdds(t *testing.T) {
	raw, err := os.ReadFile("../../../charts/laboratory/templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)laboratory\.operatorCapabilities" -\}\}\s*\[(.*?)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("the list is not in the chart helpers")
	}
	in := map[string]bool{}
	for _, c := range regexp.MustCompile(`'([A-Z_]+)'`).FindAllSubmatch(m[1], -1) {
		in[string(c[1])] = true
	}
	want := map[string]bool{}
	add := func(cs ...string) {
		for _, c := range cs {
			want[c] = true
		}
	}
	add(profiles.Base...)
	for _, id := range profiles.IDs() {
		add(profiles.Get(id).Caps...)
	}
	add(names.DHCPImpliedCapabilities...)
	add(vpnCaps...)
	add(gatewayCaps...)
	add("NET_ADMIN", "NET_RAW") // the netconfig init container
	for c := range want {
		if !in[c] {
			t.Errorf("the code can add %s, the policy would refuse the pod", c)
		}
	}
	for c := range in {
		if !want[c] {
			t.Errorf("the policy allows %s, which nothing the operator makes uses", c)
		}
	}
	for _, never := range profiles.Never {
		if in[never] {
			t.Errorf("%s is on the never list", never)
		}
	}
}
