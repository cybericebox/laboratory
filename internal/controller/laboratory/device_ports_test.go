package laboratory

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/consolespec"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/portkeeper"
)

func portsDevice() *laboratoryv1alpha1.Device {
	d := readinessDevice(nil, false)
	d.Namespace = "ns"
	d.Spec.Service = &laboratoryv1alpha1.DeviceService{Command: []string{"/usr/lib/frr/docker-start"}, User: "root"}
	d.Spec.ConsolePorts = []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0", Command: []string{"/usr/bin/vtysh"}}}
	d.Spec.SerialPorts = []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}}
	return d
}

// A device with ports always starts through the launcher, which gets the port list; the pod carries the list (annotation, read through a
// downward API volume), the digest label the device access service checks, and the memory volume of the keeper's sockets. Nothing of it
// adds a privilege: the container keeps the device's own capabilities.
func TestDeviceWithPortsStartsThroughTheLauncher(t *testing.T) {
	d := portsDevice()
	labels, _, ann, spec := readinessReconciler().workloadTemplate(d)
	main := spec.Containers[0]
	want := []string{names.ProbeBinary, "launch", "-ports", portkeeper.SpecPath, "-user", "root", "--", "/usr/lib/frr/docker-start"}
	if !slices.Equal(main.Command, want) {
		t.Fatalf("command = %v, want %v", main.Command, want)
	}
	ks := consolespec.KeeperSpec(&d.Spec)
	if ann[names.AnnotationPortsSpec] != consolespec.Marshal(ks) || labels[names.LabelPorts] != consolespec.PortsHash(ks) {
		t.Fatalf("annotation %q label %q", ann[names.AnnotationPortsSpec], labels[names.LabelPorts])
	}
	v := volumeByName(spec, names.PortsSpecVolume)
	if v == nil || v.DownwardAPI == nil || len(v.DownwardAPI.Items) != 1 || v.DownwardAPI.Items[0].Path != portkeeper.SpecFile ||
		v.DownwardAPI.Items[0].FieldRef.FieldPath != "metadata.annotations['"+names.AnnotationPortsSpec+"']" {
		t.Fatalf("port list volume %+v", v)
	}
	if m := mounts(main, names.PortsSpecVolume); len(m) != 1 || m[0].MountPath != portkeeper.SpecDir || !m[0].ReadOnly {
		t.Fatalf("port list mount %+v", m)
	}
	s := volumeByName(spec, names.PortsSocketVolume)
	if s == nil || s.EmptyDir == nil || s.EmptyDir.Medium != corev1.StorageMediumMemory || s.EmptyDir.SizeLimit == nil {
		t.Fatalf("socket volume %+v", s)
	}
	if m := mounts(main, names.PortsSocketVolume); len(m) != 1 || m[0].MountPath != portkeeper.SocketDir || m[0].ReadOnly {
		t.Fatalf("socket mount %+v", m)
	}
	if caps := main.SecurityContext.Capabilities; caps == nil || len(caps.Add) != len(deviceSecurityContext(d).Capabilities.Add) {
		t.Fatalf("the launcher must not add a capability: %+v", caps)
	}
	if main.SecurityContext.Privileged != nil && *main.SecurityContext.Privileged {
		t.Fatal("never privileged")
	}
}

// A device without ports keeps its pod as before: no label, no annotation, no volume.
func TestDeviceWithoutPortsHasNoKeeper(t *testing.T) {
	d := portsDevice()
	d.Spec.ConsolePorts, d.Spec.SerialPorts = nil, nil
	labels, _, ann, spec := readinessReconciler().workloadTemplate(d)
	if _, ok := labels[names.LabelPorts]; ok || ann[names.AnnotationPortsSpec] != "" || volumeByName(spec, names.PortsSocketVolume) != nil ||
		slices.Contains(spec.Containers[0].Command, "-ports") {
		t.Fatalf("labels %v annotations %v command %v", labels, ann, spec.Containers[0].Command)
	}
}

// The ports need the launcher: a device with ports and no service command or user, or without the node image, is refused.
func TestDeviceWithPortsRefusedWithoutTheLauncher(t *testing.T) {
	r := readinessReconciler()
	d := portsDevice()
	if err := r.refuseUnbootstrappable(d); err != nil {
		t.Fatal(err)
	}
	d.Spec.Service = nil
	if err := r.refuseUnbootstrappable(d); err == nil || !strings.Contains(err.Error(), "console or serial ports") {
		t.Fatalf("no service: %v", err)
	}
	d = portsDevice()
	d.Spec.Service.User = ""
	if err := r.refuseUnbootstrappable(d); err == nil {
		t.Fatal("no service user")
	}
	d = portsDevice()
	r.NetConfigImage = ""
	if err := r.refuseUnbootstrappable(d); err == nil {
		t.Fatal("no node image")
	}
}
