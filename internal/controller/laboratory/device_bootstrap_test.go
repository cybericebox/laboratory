package laboratory

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/launcher"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
)

func bootstrapDevice() *laboratoryv1alpha1.Device {
	d := readinessDevice(nil, false)
	d.Namespace = "ns"
	d.Spec.Bootstrap = &laboratoryv1alpha1.DeviceBootstrapSpec{Enabled: true}
	d.Spec.Service = &laboratoryv1alpha1.DeviceService{
		Command: []string{"/usr/sbin/nginx"}, Args: []string{"-g", "daemon off;"}, User: "www-data:www-data", WorkingDir: "/srv",
	}
	return d
}

func volumeByName(spec corev1.PodSpec, name string) *corev1.Volume {
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == name {
			return &spec.Volumes[i]
		}
	}
	return nil
}

func mounts(c corev1.Container, volume string) []corev1.VolumeMount {
	var out []corev1.VolumeMount
	for _, m := range c.VolumeMounts {
		if m.Name == volume {
			out = append(out, m)
		}
	}
	return out
}

// The exact shape of a bootstrapped device pod.
func TestBootstrappedPodSpec(t *testing.T) {
	d := bootstrapDevice()
	_, _, ann, spec := readinessReconciler().workloadTemplate(d)

	main := spec.Containers[0]
	wantCmd := []string{names.ProbeBinary, "launch", "-bootstrap", "-user", "www-data:www-data", "-workdir", "/srv", "--", "/usr/sbin/nginx", "-g", "daemon off;"}
	if !slices.Equal(main.Command, wantCmd) || main.Args != nil {
		t.Fatalf("command = %v args = %v", main.Command, main.Args)
	}
	if ann[names.AnnotationBootstrap] != names.BootstrapRun {
		t.Fatalf("annotations = %v", ann)
	}
	// Root, so that files can be placed; every other hardening setting is the device's own.
	if sc := main.SecurityContext; sc.RunAsUser == nil || *sc.RunAsUser != 0 || sc.RunAsNonRoot != nil || !reflect.DeepEqual(sc.Capabilities, deviceSecurityContext(d).Capabilities) {
		t.Fatalf("security context = %+v", sc)
	}
	if spec.SecurityContext.SeccompProfile == nil || spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatalf("pod hardening lost: %+v", spec.SecurityContext)
	}

	// Init containers: the probe installer, then the stager.
	var inits []string
	for _, ic := range spec.InitContainers {
		inits = append(inits, ic.Name)
	}
	if !slices.Equal(inits, []string{names.ProbeInitContainer, names.BootstrapInitContainer}) {
		t.Fatalf("init containers = %v", inits)
	}
	stage := initContainer(spec, names.BootstrapInitContainer)
	if !slices.Equal(stage.Command, []string{names.ProbeImageBinary, "stage", names.BootstrapSrcDir + "/bootstrap.json", names.BootstrapDir}) {
		t.Fatalf("stage command = %v", stage.Command)
	}
	if sc := stage.SecurityContext; sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Add) != 0 || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatalf("stage security context = %+v", sc)
	}

	// Volumes: a memory-backed emptyDir and the Secret.
	boot := volumeByName(spec, names.BootstrapVolume)
	if boot == nil || boot.EmptyDir == nil || boot.EmptyDir.Medium != corev1.StorageMediumMemory || boot.EmptyDir.SizeLimit == nil {
		t.Fatalf("boot volume = %+v", boot)
	}
	sec := volumeByName(spec, names.BootstrapSecretVolume)
	if sec == nil || sec.Secret == nil || sec.Secret.SecretName != "lab1-web-bootstrap" || sec.Secret.Optional != nil || *sec.Secret.DefaultMode != 0o400 {
		t.Fatalf("secret volume = %+v", sec)
	}

	// Probes.
	if p := main.StartupProbe; p == nil || !slices.Equal(p.Exec.Command, []string{names.ProbeBinary, "ready", "-file", names.BootstrapSettledFile}) {
		t.Fatalf("startup probe = %+v", main.StartupProbe)
	}
	if p := main.ReadinessProbe; p == nil || !slices.Equal(p.Exec.Command, []string{names.ProbeBinary, "ready", "-file", names.BootstrapOKFile}) {
		t.Fatalf("with no condition of its own, Ready follows the bootstrap: %+v", main.ReadinessProbe)
	}
}

// The Secret reaches no container but the stager, and no bootstrap value is in any environment or command.
func TestBootstrapSecretIsMountedOnlyInTheStager(t *testing.T) {
	_, _, _, spec := readinessReconciler().workloadTemplate(bootstrapDevice())
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		got := mounts(c, names.BootstrapSecretVolume)
		if c.Name == names.BootstrapInitContainer {
			if len(got) != 1 || !got[0].ReadOnly {
				t.Fatalf("the stager must mount the Secret read-only: %+v", got)
			}
			continue
		}
		if len(got) != 0 {
			t.Fatalf("container %s mounts the bootstrap Secret", c.Name)
		}
		for _, e := range c.EnvFrom {
			if e.SecretRef != nil && strings.Contains(e.SecretRef.Name, "bootstrap") {
				t.Fatalf("container %s takes the bootstrap Secret as environment", c.Name)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && strings.Contains(e.ValueFrom.SecretKeyRef.Name, "bootstrap") {
				t.Fatalf("container %s reads the bootstrap Secret into a variable", c.Name)
			}
		}
	}
	// The device container sees only the memory volume that the launcher empties, and the program.
	main := spec.Containers[0]
	if len(mounts(main, names.BootstrapVolume)) != 1 || mounts(main, names.BootstrapVolume)[0].ReadOnly {
		t.Fatalf("main mounts = %+v", main.VolumeMounts)
	}
	for _, v := range spec.Volumes {
		if v.Secret != nil && v.Name != names.BootstrapSecretVolume {
			if strings.Contains(v.Secret.SecretName, "bootstrap") {
				t.Fatalf("another volume carries the bootstrap Secret: %+v", v)
			}
		}
	}
}

func TestReadinessNeedsBootstrapAndTheDevicesCondition(t *testing.T) {
	d := bootstrapDevice()
	d.Spec.WaitReadyFile = true
	d.Spec.Access = []laboratoryv1alpha1.DeviceAccess{{Kind: laboratoryv1alpha1.AccessHTTP, Port: 8080}}
	_, _, _, spec := readinessReconciler().workloadTemplate(d)
	want := []string{names.ProbeBinary, "ready", "-file", names.BootstrapOKFile + ":" + names.ReadyFile, "-port", "8080", "-scheme", "http"}
	if got := spec.Containers[0].ReadinessProbe.Exec.Command; !slices.Equal(got, want) {
		t.Fatalf("readiness = %v, want %v", got, want)
	}
}

// A device without a bootstrap and without a service command gets exactly the pod it gets today.
func TestDeviceWithoutBootstrapOrServiceIsUnchanged(t *testing.T) {
	r := readinessReconciler()
	plain := readinessDevice(&laboratoryv1alpha1.DeviceAccess{Kind: laboratoryv1alpha1.AccessHTTP, Port: 80}, true)
	_, _, annPlain, specPlain := r.workloadTemplate(plain)
	if specPlain.Containers[0].Command != nil || specPlain.Containers[0].StartupProbe != nil || specPlain.Containers[0].SecurityContext.RunAsUser != nil {
		t.Fatalf("a plain device must keep the image's entrypoint and user: %+v", specPlain.Containers[0])
	}
	for _, v := range specPlain.Volumes {
		if v.Name == names.BootstrapVolume || v.Name == names.BootstrapSecretVolume {
			t.Fatalf("volume %s on a plain device", v.Name)
		}
	}
	if _, ok := annPlain[names.AnnotationBootstrap]; ok {
		t.Fatal("annotation on a plain device")
	}
	// A service block that names no command or arguments changes nothing either: the image decides.
	withUser := readinessDevice(&laboratoryv1alpha1.DeviceAccess{Kind: laboratoryv1alpha1.AccessHTTP, Port: 80}, true)
	withUser.Spec.Service = &laboratoryv1alpha1.DeviceService{User: "app", WorkingDir: "/app"}
	_, _, annUser, specUser := r.workloadTemplate(withUser)
	if !reflect.DeepEqual(specPlain, specUser) || !reflect.DeepEqual(annPlain, annUser) {
		t.Fatal("a service block without a command must not change the pod")
	}
}

func TestServiceCommandOverrideWithoutBootstrap(t *testing.T) {
	d := readinessDevice(nil, false)
	d.Spec.Service = &laboratoryv1alpha1.DeviceService{Command: []string{"/app"}, Args: []string{"--x"}, User: "1000"}
	_, _, ann, spec := readinessReconciler().workloadTemplate(d)
	c := spec.Containers[0]
	if !slices.Equal(c.Command, []string{names.ProbeBinary, "launch", "-user", "1000", "--", "/app", "--x"}) {
		t.Fatalf("command = %v", c.Command)
	}
	if c.StartupProbe != nil || volumeByName(spec, names.BootstrapSecretVolume) != nil || volumeByName(spec, names.BootstrapVolume) != nil || initContainer(spec, names.BootstrapInitContainer) != nil {
		t.Fatal("an override alone brings no bootstrap delivery")
	}
	if volumeByName(spec, names.ProbeVolume) == nil || initContainer(spec, names.ProbeInitContainer) == nil {
		t.Fatal("the launcher must be installed")
	}
	if _, ok := ann[names.AnnotationBootstrap]; ok {
		t.Fatal("no bootstrap annotation without a bootstrap")
	}
}

// A pod that starts from a saved state skips the bootstrap: no stager, no Secret, the launcher only starts the service.
func TestRestoredPodSkipsTheBootstrap(t *testing.T) {
	d := bootstrapDevice()
	_, _, ann, spec := readinessReconciler().workloadTemplateFor(d, true, podPlan{Restored: true})
	c := spec.Containers[0]
	if slices.Contains(c.Command, "-bootstrap") || ann[names.AnnotationBootstrap] != names.BootstrapSkip {
		t.Fatalf("command = %v annotations = %v", c.Command, ann)
	}
	if volumeByName(spec, names.BootstrapSecretVolume) != nil || volumeByName(spec, names.BootstrapVolume) != nil || initContainer(spec, names.BootstrapInitContainer) != nil || c.StartupProbe != nil {
		t.Fatal("a skipped bootstrap must not deliver the list")
	}
	if p := c.ReadinessProbe; p != nil {
		t.Fatalf("no condition of its own: nothing to wait for, got %+v", p)
	}
	if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Fatal("the launcher drops the user itself, so it starts as root")
	}
}

func TestRescuePodHasNoLauncher(t *testing.T) {
	_, _, ann, spec := readinessReconciler().workloadTemplateFor(bootstrapDevice(), true, podPlan{Restored: true, Rescue: true})
	if spec.Containers[0].Command != nil || initContainer(spec, names.BootstrapInitContainer) != nil || ann[names.AnnotationBootstrap] != "" {
		t.Fatalf("rescue starts a shell and bootstraps nothing: %+v", spec.Containers[0])
	}
}

func TestStatePodCarriesItsAttempt(t *testing.T) {
	_, _, ann, spec := readinessReconciler().workloadTemplateFor(bootstrapDevice(), true, podPlan{Attempt: 2})
	if !slices.Contains(spec.Containers[0].Command, "-attempt") || ann[names.AnnotationBootstrapAttempt] != "2" {
		t.Fatalf("%v %v", spec.Containers[0].Command, ann)
	}
}

func TestBootstrapRefusedWithoutAServiceCommandOrLauncher(t *testing.T) {
	r := readinessReconciler()
	d := bootstrapDevice()
	d.Spec.Service = nil
	if err := r.refuseUnbootstrappable(d); err == nil || !strings.Contains(err.Error(), "service command is unknown") {
		t.Fatalf("err = %v", err)
	}
	d.Spec.Service = &laboratoryv1alpha1.DeviceService{User: "x"}
	if err := r.refuseUnbootstrappable(d); err == nil {
		t.Fatal("a user without a command is not a command")
	}
	d = bootstrapDevice()
	if err := r.refuseUnbootstrappable(d); err != nil {
		t.Fatal(err)
	}
	r.NetConfigImage = ""
	if err := r.refuseUnbootstrappable(d); err == nil {
		t.Fatal("no node image, no launcher")
	}
	plain := readinessDevice(nil, false)
	if err := r.refuseUnbootstrappable(plain); err != nil {
		t.Fatal("a device without a bootstrap is never refused")
	}
}

func TestRefusalIsRecordedOnTheDevice(t *testing.T) {
	d := bootstrapDevice()
	d.Spec.Service = nil
	s := pruneScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(d).WithStatusSubresource(&laboratoryv1alpha1.Device{}).Build()
	r := &DeviceReconciler{Client: c, Scheme: s, NetConfigImage: "registry/node:1"}
	err := r.refuseUnbootstrappable(d)
	handled, serr := r.handleRefusal(context.Background(), d, err)
	if !handled || serr != nil {
		t.Fatalf("handled %v err %v", handled, serr)
	}
	var got laboratoryv1alpha1.Device
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), &got); err != nil {
		t.Fatal(err)
	}
	if b := got.Status.Bootstrap; b == nil || b.Phase != laboratoryv1alpha1.BootstrapFailed || !strings.Contains(b.Reason, "service command is unknown") {
		t.Fatalf("status = %+v", b)
	}
	if handled, _ := r.handleRefusal(context.Background(), d, errProfileUnsupported); handled {
		t.Fatal("other errors are not refusals of the bootstrap")
	}
}

// A micro-VM device gets the same scheme, with the differences it has today (its own kernel: no user namespace, no TUN).
func TestMicroVMDeviceBootstrapsLikeAContainer(t *testing.T) {
	r := readinessReconciler()
	r.Profiles = nodecap.ProfilePolicy{MicroVM: true}
	r.MicroVMRuntimeClass = "kata-clh"
	r.Security = PodSecurity{UserNamespaces: true}
	d := bootstrapDevice()
	d.Spec.Profile = laboratoryv1alpha1.DeviceProfile(nodecap.ProfileMicroVM)
	_, _, ann, spec := r.workloadTemplate(d)
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != "kata-clh" || spec.HostUsers != nil {
		t.Fatalf("microVM specifics lost: runtime %v hostUsers %v", spec.RuntimeClassName, spec.HostUsers)
	}
	if ann[names.AnnotationBootstrap] != names.BootstrapRun || initContainer(spec, names.BootstrapInitContainer) == nil || volumeByName(spec, names.BootstrapVolume) == nil {
		t.Fatal("the same delivery applies to a microVM")
	}
	// A container device in the same cluster is in a user namespace.
	d.Spec.Profile = ""
	_, _, _, spec = r.workloadTemplate(d)
	if spec.HostUsers == nil || *spec.HostUsers {
		t.Fatal("container devices keep their user namespace")
	}
}

func container(name string, started bool, state, last corev1.ContainerState) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: name, Started: &started, State: state, LastTerminationState: last}}}}
}

func terminated(msg string, code int32) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: msg}}
}

var running = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}

func TestObserveBootstrap(t *testing.T) {
	failed1 := `{"kind":"failed","attempt":1,"reason":"the script exited with code 3"}`
	failed3 := `{"kind":"failed","attempt":3,"final":true,"reason":"the script exited with code 3"}`
	recycle := `{"kind":"recycle","reason":"x"}`
	for _, tc := range []struct {
		name    string
		pod     *corev1.Pod
		phase   laboratoryv1alpha1.BootstrapPhase
		attempt int32
		reason  string
		recycle bool
	}{
		{"not started", &corev1.Pod{}, laboratoryv1alpha1.BootstrapPending, 0, "", false},
		{"running, not settled", container("web", false, running, corev1.ContainerState{}), laboratoryv1alpha1.BootstrapPending, 0, "", false},
		{"settled", container("web", true, running, corev1.ContainerState{}), laboratoryv1alpha1.BootstrapDone, 1, "", false},
		{"settled after a failed attempt", container("web", true, running, terminated(failed1, 78)), laboratoryv1alpha1.BootstrapDone, 2, "", false},
		{"first attempt failed", container("web", false, terminated(failed1, 78), corev1.ContainerState{}), laboratoryv1alpha1.BootstrapPending, 1, "the script exited with code 3", false},
		{"waiting for the next attempt", container("web", false, corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}, terminated(failed1, 78)), laboratoryv1alpha1.BootstrapPending, 1, "the script exited with code 3", false},
		{"gave up, waiting", container("web", true, running, terminated(failed3, 78)), laboratoryv1alpha1.BootstrapFailed, 3, "the script exited with code 3", false},
		{"gave up, pod ended", container("web", false, terminated(failed3, 78), corev1.ContainerState{}), laboratoryv1alpha1.BootstrapFailed, 3, "the script exited with code 3", false},
		{"service ended, restarted in place", container("web", false, corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}, terminated(recycle, 79)), laboratoryv1alpha1.BootstrapDone, 0, "", true},
		{"killed without a message", container("web", false, corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}, terminated("", 137)), laboratoryv1alpha1.BootstrapPending, 0, "", false},
		{"another container's message", container("other", true, running, corev1.ContainerState{}), laboratoryv1alpha1.BootstrapPending, 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := observeBootstrap(tc.pod, "web")
			if o.Phase != tc.phase || o.Attempts != tc.attempt || o.Reason != tc.reason || o.Recycle != tc.recycle {
				t.Fatalf("observed %+v", o)
			}
		})
	}
}

func TestBootstrapReasonsNeverCarryValues(t *testing.T) {
	long := strings.Repeat("x", 1000)
	m := `{"kind":"failed","attempt":3,"final":true,"reason":"` + long + `"}`
	o := observeBootstrap(container("web", true, running, terminated(m, 78)), "web")
	if len(o.Reason) > 256 {
		t.Fatalf("reason of %d bytes", len(o.Reason))
	}
}

func devicePod(device string, annotations map[string]string, cs ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-" + device, Namespace: "ns", Annotations: annotations,
			Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: device},
		},
		Status: corev1.PodStatus{ContainerStatuses: cs},
	}
}

func statusReconciler(t *testing.T, objs ...client.Object) (*DeviceReconciler, client.Client) {
	t.Helper()
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&laboratoryv1alpha1.Device{}).Build()
	return &DeviceReconciler{Client: c, Scheme: s, NetConfigImage: "registry/node:1"}, c
}

// When the service of a Deployment pod ends, the kubelet restarts the container in place; the controller recreates the pod.
func TestRestartedBootstrappedContainerRecreatesThePod(t *testing.T) {
	d := bootstrapDevice()
	recycle := `{"kind":"recycle"}`
	pod := devicePod("web", map[string]string{names.AnnotationBootstrap: names.BootstrapRun},
		corev1.ContainerStatus{Name: "web", Started: ptrBool(false), State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}, LastTerminationState: terminated(recycle, 79)})
	r, c := statusReconciler(t, d, pod)
	st, err := r.workloadBootstrap(context.Background(), d)
	if err != nil || st.Phase != laboratoryv1alpha1.BootstrapDone {
		t.Fatalf("status %+v err %v", st, err)
	}
	var got corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &got); err == nil {
		t.Fatal("the pod must be deleted so that a new one runs the init containers again")
	}
}

// A device that gave up, or one still retrying in place, keeps its pod.
func TestFailedOrRetryingBootstrapKeepsThePod(t *testing.T) {
	for _, msg := range []string{
		`{"kind":"failed","attempt":1,"reason":"r"}`,
		`{"kind":"failed","attempt":3,"final":true,"reason":"r"}`,
	} {
		d := bootstrapDevice()
		pod := devicePod("web", map[string]string{names.AnnotationBootstrap: names.BootstrapRun},
			corev1.ContainerStatus{Name: "web", Started: ptrBool(false), State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}, LastTerminationState: terminated(msg, 78)})
		r, c := statusReconciler(t, d, pod)
		if _, err := r.workloadBootstrap(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		var got corev1.Pod
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &got); err != nil {
			t.Fatalf("%s: the pod must stay (no restart loop): %v", msg, err)
		}
	}
}

func TestDeploymentDeviceWithoutPodIsPending(t *testing.T) {
	d := bootstrapDevice()
	r, _ := statusReconciler(t, d)
	st, err := r.workloadBootstrap(context.Background(), d)
	if err != nil || st.Phase != laboratoryv1alpha1.BootstrapPending || st.Attempts != 0 {
		t.Fatalf("%+v %v", st, err)
	}
	if st, _ := r.workloadBootstrap(context.Background(), readinessDevice(nil, false)); st != nil {
		t.Fatal("a device without a bootstrap has no bootstrap status")
	}
}

func TestPodMapperSelectsOnlyBootstrapPods(t *testing.T) {
	r := &DeviceReconciler{}
	if got := r.devicesForBootstrapPod(context.Background(), devicePod("web", nil)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	got := r.devicesForBootstrapPod(context.Background(), devicePod("web", map[string]string{names.AnnotationBootstrap: names.BootstrapRun}))
	if len(got) != 2 || !strings.HasPrefix(got[0].Name, "lab1-web-") || got[1].Name != "lab1-web" {
		t.Fatalf("%v", got)
	}
}

func TestPlanForStatePod(t *testing.T) {
	dev := func(phase laboratoryv1alpha1.BootstrapPhase, attempts int32) *laboratoryv1alpha1.Device {
		d := bootstrapDevice()
		if phase != "" {
			d.Status.Bootstrap = &laboratoryv1alpha1.DeviceBootstrapStatus{Phase: phase, Attempts: attempts}
		}
		return d
	}
	for _, tc := range []struct {
		name         string
		d            *laboratoryv1alpha1.Device
		hasImage     bool
		rescue       bool
		restored     bool
		attempt      int32
		discard      bool
		launcherMode launchMode
	}{
		{"first start", dev("", 0), false, false, false, 1, false, launchRun},
		{"restore after done", dev(laboratoryv1alpha1.BootstrapDone, 1), true, false, true, 0, false, launchSkip},
		{"done but no state saved: clean again", dev(laboratoryv1alpha1.BootstrapDone, 1), false, false, false, 1, false, launchRun},
		{"state of an unfinished bootstrap is discarded", dev(laboratoryv1alpha1.BootstrapPending, 1), true, false, false, 2, true, launchRun},
		{"second attempt", dev(laboratoryv1alpha1.BootstrapPending, 1), false, false, false, 2, false, launchRun},
		{"rescue never bootstraps", dev(laboratoryv1alpha1.BootstrapPending, 1), true, true, true, 0, false, launchNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, discard := planForStatePod(tc.d, tc.hasImage, tc.rescue)
			if plan.Restored != tc.restored || plan.Attempt != tc.attempt || discard != tc.discard || launchModeFor(tc.d, plan) != tc.launcherMode {
				t.Fatalf("plan %+v discard %v mode %v", plan, discard, launchModeFor(tc.d, plan))
			}
		})
	}
	// A device without a bootstrap restores whatever it saved, as before.
	plain := readinessDevice(nil, false)
	if plan, discard := planForStatePod(plain, true, false); !plan.Restored || discard || plan.Attempt != 0 {
		t.Fatalf("%+v %v", plan, discard)
	}
}

// How a pod of a device with state persistence ends before its bootstrap finished: one more failed attempt, counted once.
func TestEndedStatePodCountsOneAttempt(t *testing.T) {
	msg := `{"kind":"failed","attempt":2,"reason":"the script exited with code 1"}`
	pod := devicePod("web", map[string]string{names.AnnotationBootstrap: names.BootstrapRun, names.AnnotationBootstrapAttempt: "2"},
		corev1.ContainerStatus{Name: "web", Started: ptrBool(false), State: terminated(msg, 78)})
	d := bootstrapDevice()
	d.Status.Bootstrap = &laboratoryv1alpha1.DeviceBootstrapStatus{Phase: laboratoryv1alpha1.BootstrapPending, Attempts: 1}
	r, c := statusReconciler(t, d, pod)
	for range 2 { // the same pod seen twice
		if err := r.recordEndedBootstrapPod(context.Background(), d, pod); err != nil {
			t.Fatal(err)
		}
	}
	var got laboratoryv1alpha1.Device
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(d), &got)
	if b := got.Status.Bootstrap; b.Phase != laboratoryv1alpha1.BootstrapPending || b.Attempts != 2 || b.Reason != "the script exited with code 1" {
		t.Fatalf("%+v", b)
	}
	// The third attempt fails the device, even when the container just vanished (no message).
	pod3 := devicePod("web", map[string]string{names.AnnotationBootstrap: names.BootstrapRun, names.AnnotationBootstrapAttempt: "3"},
		corev1.ContainerStatus{Name: "web", Started: ptrBool(false), State: terminated("", 137)})
	if err := r.recordEndedBootstrapPod(context.Background(), &got, pod3); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(d), &got)
	if b := got.Status.Bootstrap; b.Phase != laboratoryv1alpha1.BootstrapFailed || b.Attempts != 3 || b.Reason == "" {
		t.Fatalf("%+v", b)
	}
	if !bootstrapFailed(&got) {
		t.Fatal("a failed device is held")
	}
	// A pod whose bootstrap had succeeded ends for another reason: no attempt.
	done := bootstrapDevice()
	done.Status.Bootstrap = &laboratoryv1alpha1.DeviceBootstrapStatus{Phase: laboratoryv1alpha1.BootstrapDone, Attempts: 1}
	r2, _ := statusReconciler(t, done)
	if err := r2.recordEndedBootstrapPod(context.Background(), done, pod3); err != nil || done.Status.Bootstrap.Phase != laboratoryv1alpha1.BootstrapDone {
		t.Fatalf("%+v %v", done.Status.Bootstrap, err)
	}
}

func TestSyncStateBootstrapKeepsTheCountUntilThePodSpeaks(t *testing.T) {
	d := bootstrapDevice()
	d.Status.Bootstrap = &laboratoryv1alpha1.DeviceBootstrapStatus{Phase: laboratoryv1alpha1.BootstrapPending, Attempts: 1, Reason: "r"}
	pod := devicePod("web", map[string]string{names.AnnotationBootstrap: names.BootstrapRun, names.AnnotationBootstrapAttempt: "2"},
		corev1.ContainerStatus{Name: "web", Started: ptrBool(false), State: running})
	r, _ := statusReconciler(t, d, pod)
	if err := r.syncStateBootstrap(context.Background(), d, pod); err != nil {
		t.Fatal(err)
	}
	if d.Status.Bootstrap.Attempts != 1 || d.Status.Bootstrap.Reason != "r" {
		t.Fatalf("%+v", d.Status.Bootstrap)
	}
	pod.Status.ContainerStatuses[0].Started = ptrBool(true)
	if err := r.syncStateBootstrap(context.Background(), d, pod); err != nil {
		t.Fatal(err)
	}
	if b := d.Status.Bootstrap; b.Phase != laboratoryv1alpha1.BootstrapDone || b.Reason != "" {
		t.Fatalf("%+v", b)
	}
}

// The attempt limit and the pauses are the ones of the contract.
func TestAttemptLimitMatchesTheContract(t *testing.T) {
	if labchecks.BootstrapMaxAttempts != 3 || launcher.ExitFailed == launcher.ExitRecycle {
		t.Fatal("contract")
	}
	if labchecks.BootstrapPause(1) >= labchecks.BootstrapPause(2) || labchecks.BootstrapPause(0) != 0 {
		t.Fatal("pauses must grow")
	}
}
