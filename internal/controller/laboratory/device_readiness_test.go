package laboratory

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func readinessDevice(web *laboratoryv1alpha1.WebExposure, waitFile bool) *laboratoryv1alpha1.Device {
	d := &laboratoryv1alpha1.Device{}
	d.Name, d.Spec.Name, d.Spec.LabRef = "lab1-web", "web", "lab1"
	d.Spec.Type = laboratoryv1alpha1.DeviceTypeContainer
	d.Spec.Image = "nginx:1"
	d.Spec.WaitReadyFile = waitFile
	if web != nil {
		d.Spec.Exposure = &laboratoryv1alpha1.ExposureSpec{Web: web}
	}
	return d
}

func readinessReconciler() *DeviceReconciler {
	return &DeviceReconciler{NetConfigImage: "registry/node:1", Defaults: testDefaults}
}

func mainContainer(spec corev1.PodSpec) corev1.Container { return spec.Containers[0] }

func initContainer(spec corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == name {
			return &spec.InitContainers[i]
		}
	}
	return nil
}

func TestDeviceWithoutPortOrMarkerHasNoProbe(t *testing.T) {
	_, _, _, spec := readinessReconciler().workloadTemplate(readinessDevice(nil, false), false)
	if mainContainer(spec).ReadinessProbe != nil || initContainer(spec, names.ProbeInitContainer) != nil || len(spec.Volumes) != 0 {
		t.Fatalf("a device that publishes nothing and waits for nothing keeps its pod as it was: %+v", spec)
	}
}

func TestProxiedPortGetsAnAutomaticProbe(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, scheme string
	}{{"http", "http", "http"}, {"https", "https", "https"}, {"default is http", "", "http"}, {"other is tcp", "grpc", "tcp"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, spec := readinessReconciler().workloadTemplate(readinessDevice(&laboratoryv1alpha1.WebExposure{Port: 8080, Protocol: tc.protocol}, false), false)
			p := mainContainer(spec).ReadinessProbe
			if p == nil || p.Exec == nil {
				t.Fatalf("readiness probe = %+v", p)
			}
			want := []string{names.ProbeBinary, "ready", "-port", "8080", "-scheme", tc.scheme}
			if !slices.Equal(p.Exec.Command, want) {
				t.Fatalf("command = %v, want %v", p.Exec.Command, want)
			}
		})
	}
}

func TestMarkerFileProbe(t *testing.T) {
	_, _, _, spec := readinessReconciler().workloadTemplate(readinessDevice(nil, true), false)
	p := mainContainer(spec).ReadinessProbe
	if p == nil || p.Exec == nil {
		t.Fatalf("readiness probe = %+v", p)
	}
	want := []string{names.ProbeBinary, "ready", "-file", names.ReadyFile}
	if !slices.Equal(p.Exec.Command, want) {
		t.Fatalf("command = %v, want %v", p.Exec.Command, want)
	}
}

func TestMarkerAndPortProbeChecksBoth(t *testing.T) {
	_, _, _, spec := readinessReconciler().workloadTemplate(readinessDevice(&laboratoryv1alpha1.WebExposure{Port: 443, Protocol: "https"}, true), false)
	want := []string{names.ProbeBinary, "ready", "-file", names.ReadyFile, "-port", "443", "-scheme", "https"}
	if got := mainContainer(spec).ReadinessProbe.Exec.Command; !slices.Equal(got, want) {
		t.Fatalf("command = %v, want %v", got, want)
	}
}

// Readiness only ever withholds traffic: it never kills (no liveness probe, no startup probe) and it is generous.
func TestProbeTimingAndNoKilling(t *testing.T) {
	_, _, _, spec := readinessReconciler().workloadTemplate(readinessDevice(nil, true), false)
	c := mainContainer(spec)
	p := c.ReadinessProbe
	if p.InitialDelaySeconds != 0 || p.PeriodSeconds != 2 || p.SuccessThreshold != 1 || p.FailureThreshold < 10 || p.TimeoutSeconds < 2 {
		t.Fatalf("timing = %+v", p)
	}
	if c.LivenessProbe != nil || c.StartupProbe != nil {
		t.Fatalf("a readiness probe must not come with a liveness/startup probe: %+v / %+v", c.LivenessProbe, c.StartupProbe)
	}
}

func TestProbeInstallPlumbing(t *testing.T) {
	_, _, _, spec := readinessReconciler().workloadTemplate(readinessDevice(nil, true), false)
	ic := initContainer(spec, names.ProbeInitContainer)
	if ic == nil {
		t.Fatalf("init containers = %+v", spec.InitContainers)
	}
	if ic.Image != "registry/node:1" || !slices.Equal(ic.Command, []string{names.ProbeImageBinary, "install", names.ProbeDir}) {
		t.Fatalf("installer = image %q command %v", ic.Image, ic.Command)
	}
	if len(ic.VolumeMounts) != 1 || ic.VolumeMounts[0].Name != names.ProbeVolume || ic.VolumeMounts[0].MountPath != names.ProbeDir || ic.VolumeMounts[0].ReadOnly {
		t.Fatalf("installer mounts = %+v", ic.VolumeMounts)
	}
	if sc := ic.SecurityContext; sc == nil || sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0 ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatalf("installer security context = %+v", sc)
	}
	if ic.Resources.Requests.Cpu().Cmp(*ic.Resources.Limits.Cpu()) != 0 || ic.Resources.Requests.Memory().Cmp(*ic.Resources.Limits.Memory()) != 0 {
		t.Fatalf("requests must equal limits so the pod stays Guaranteed: %+v", ic.Resources)
	}
	var vol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == names.ProbeVolume {
			vol = &spec.Volumes[i]
		}
	}
	if vol == nil || vol.EmptyDir == nil || vol.EmptyDir.SizeLimit == nil {
		t.Fatalf("volume = %+v", vol)
	}
	mounts := mainContainer(spec).VolumeMounts
	if len(mounts) != 1 || mounts[0].Name != names.ProbeVolume || mounts[0].MountPath != names.ProbeDir || !mounts[0].ReadOnly {
		t.Fatalf("the device sees the probe read-only, got %+v", mounts)
	}
}

func TestProbeInstallerRunsAfterNetconfig(t *testing.T) {
	d := readinessDevice(nil, true)
	d.Spec.Interfaces = []laboratoryv1alpha1.InterfaceSpec{{Name: "eth0", Addr: &laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic, IP: "10.0.0.2/24"}}}
	_, _, _, spec := readinessReconciler().workloadTemplate(d, false)
	var order []string
	for _, ic := range spec.InitContainers {
		order = append(order, ic.Name)
	}
	if !slices.Equal(order, []string{"netconfig", names.ProbeInitContainer}) {
		t.Fatalf("init containers = %v", order)
	}
}

// Without the node image there is nothing to install the probe from: the device runs as before rather than never ready.
func TestNoProbeWithoutTheNodeImage(t *testing.T) {
	r := &DeviceReconciler{Defaults: testDefaults}
	_, _, _, spec := r.workloadTemplate(readinessDevice(&laboratoryv1alpha1.WebExposure{Port: 80, Protocol: "http"}, true), false)
	if mainContainer(spec).ReadinessProbe != nil || len(spec.InitContainers) != 0 || len(spec.Volumes) != 0 {
		t.Fatalf("pod = %+v", spec)
	}
}
