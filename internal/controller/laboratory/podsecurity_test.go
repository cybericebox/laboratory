package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
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
		t.Error("user namespaces are off unless asked for")
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
