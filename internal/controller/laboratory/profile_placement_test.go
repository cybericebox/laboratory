package laboratory

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
)

func deviceOf(profile laboratoryv1alpha1.DeviceProfile) *laboratoryv1alpha1.Device {
	return &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns"},
		Spec:       laboratoryv1alpha1.DeviceSpec{LabRef: "lab", Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx", Profile: profile},
	}
}

func TestDevicePodProfile(t *testing.T) {
	base := map[string]string{"pool": "lab"}
	r := &DeviceReconciler{LabNodeSelector: base, Profiles: nodecap.ProfilePolicy{MicroVM: true}, MicroVMRuntimeClass: "cybericebox-microvm"}

	_, _, _, spec := r.workloadTemplate(deviceOf(""), false)
	if spec.RuntimeClassName != nil || len(spec.NodeSelector) != 1 || spec.NodeSelector["pool"] != "lab" {
		t.Fatalf("a container device schedules as before (profile labels off): %v %v", spec.RuntimeClassName, spec.NodeSelector)
	}

	_, _, _, spec = r.workloadTemplate(deviceOf(laboratoryv1alpha1.DeviceProfileMicroVM), false)
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != "cybericebox-microvm" {
		t.Fatalf("runtimeClassName: %v", spec.RuntimeClassName)
	}
	if spec.NodeSelector["pool"] != "lab" || spec.NodeSelector[names.LabelProfileMicroVM] != "true" {
		t.Fatalf("selector: %v", spec.NodeSelector)
	}
	if len(base) != 1 {
		t.Fatalf("the shared lab selector must not be changed: %v", base)
	}

	// no user namespace of the host and no TUN resource for a microVM (it has its own kernel); a container keeps both
	r.Security.UserNamespaces = true
	_, _, _, spec = r.workloadTemplate(deviceOf(laboratoryv1alpha1.DeviceProfileMicroVM), false)
	if spec.HostUsers != nil {
		t.Fatalf("a microVM pod must not set hostUsers: %v", *spec.HostUsers)
	}
	_, _, _, spec = r.workloadTemplate(deviceOf(""), false)
	if spec.HostUsers == nil || *spec.HostUsers {
		t.Fatal("a container device keeps its user namespace")
	}

	r.Profiles.Labels = true
	_, _, _, spec = r.workloadTemplate(deviceOf(""), false)
	if spec.NodeSelector[names.LabelProfileContainer] != "true" || spec.RuntimeClassName != nil {
		t.Fatalf("profile labels on: a container device selects its label: %v", spec.NodeSelector)
	}
}

// The scheduler places a microVM pod only in a room that runs it, counts its overhead, and keeps container pods off nodes that
// do not carry the container label when the profile labels are on.
func TestSchedulerRoomsFollowTheProfile(t *testing.T) {
	policy := nodecap.ProfilePolicy{Labels: true, MicroVM: true, ScaleUp: []string{"container"}}
	node := func(name string, labels map[string]string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		}}
	}
	nodes := []corev1.Node{
		node("plain", map[string]string{names.LabelProfileContainer: "true"}),
		node("kvm", map[string]string{names.LabelProfileContainer: "true", names.LabelProfileMicroVM: "true", names.LabelRuntimeKataCLH: "true"}),
		node("micro-only", map[string]string{names.LabelProfileMicroVM: "true", names.LabelRuntimeKataCLH: "true"}),
	}
	c := snapshotCapacity(nodes, nil, nil, nil, amount{}, 0, nodecap.PackingReserve{})
	c.profiles = policy
	rooms := c.rooms()
	want := map[string][2]bool{"plain": {false, true}, "kvm": {false, false}, "micro-only": {true, false}} // {noContainer, noMicroVM}
	for _, r := range rooms {
		if w := want[r.Name]; r.noContainer != w[0] || r.noMicroVM != w[1] {
			t.Errorf("%s: noContainer=%v noMicroVM=%v, want %v", r.Name, r.noContainer, r.noMicroVM, w)
		}
	}

	micro := podNeed{Key: "m", CPU: 1000, Memory: 1 << 30, Profile: nodecap.ProfileMicroVM}
	left, ok := fitsGang(rooms, []podNeed{micro})
	if !ok {
		t.Fatal("a microVM pod fits the kvm node")
	}
	for _, r := range left {
		if r.Name == "plain" && r.CPU != 4000 {
			t.Errorf("a microVM pod must not take the room of a node that cannot run it: %+v", r)
		}
	}
	// only the plain node is left: the microVM pod fits nowhere
	if _, ok := fitsGang(rooms[:1], []podNeed{micro}); ok {
		t.Fatal("a microVM pod must not be placed on a node without the microVM labels")
	}
	// a container pod does not take the micro-only node
	if _, ok := fitsGang(rooms[2:3], []podNeed{{Key: "c", CPU: 1000, Memory: 1 << 30}}); ok {
		t.Fatal("with the profile labels on a container pod needs the container label")
	}
}

// An unrunnable profile never starts, on any path that makes a pod, and never falls back to a plain container: microvm without its
// RuntimeClass, microvm switched off, vm. The isolation exemptions follow the RuntimeClass actually applied, not the profile asked for.
func TestUnrunnableProfileFailsClosedOnEveryPodPath(t *testing.T) {
	ctx := context.Background()
	for name, r := range map[string]*DeviceReconciler{
		"microvm requested, no RuntimeClass configured": {Profiles: nodecap.ProfilePolicy{MicroVM: true}},
		"microvm requested, profile off":                {MicroVMRuntimeClass: "cybericebox-microvm"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := record.NewFakeRecorder(10)
			r.Recorder = rec
			d := deviceOf(laboratoryv1alpha1.DeviceProfileMicroVM)
			if err := r.createDeployment(ctx, d, 1); !errors.Is(err, errProfileUnsupported) {
				t.Errorf("createDeployment: %v", err)
			}
			if err := r.createDevicePod(ctx, d); !errors.Is(err, errProfileUnsupported) {
				t.Errorf("createDevicePod (state persistence): %v", err)
			}
			if ev := <-rec.Events; !strings.Contains(ev, "ProfileUnsupported") {
				t.Errorf("event: %q", ev)
			}
			// the spec itself never gets weaker: no RuntimeClass, so the user namespace and the TUN resource stay
			r.Security.UserNamespaces = true
			_, _, _, spec := r.workloadTemplate(d, false)
			if spec.RuntimeClassName != nil || spec.HostUsers == nil || *spec.HostUsers {
				t.Errorf("without a RuntimeClass the pod keeps its user namespace: %v %v", spec.RuntimeClassName, spec.HostUsers)
			}
		})
	}
	r := &DeviceReconciler{Recorder: record.NewFakeRecorder(10)}
	if err := r.createDeployment(ctx, deviceOf("vm"), 1); !errors.Is(err, errProfileUnsupported) {
		t.Errorf("vm: %v", err)
	}
	ok := &DeviceReconciler{Profiles: nodecap.ProfilePolicy{MicroVM: true}, MicroVMRuntimeClass: "c"}
	if !ok.profileRunnable("microvm") || !ok.profileRunnable("") {
		t.Error("microvm with its RuntimeClass, and a container, run")
	}
}

func placementPod(name string, phase corev1.PodPhase, ip string, init bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{names.LabelLab: "lab", names.LabelDevice: "client"}},
		Spec:       corev1.PodSpec{NodeName: "worker", Containers: []corev1.Container{{Name: "c"}}},
		Status:     corev1.PodStatus{Phase: phase, PodIP: ip},
	}
	if init {
		p.Spec.InitContainers = []corev1.Container{{Name: "netconfig"}}
	}
	return p
}

// A device whose netconfig init container asks the DHCP server for its address (dhcp-preset) is Pending until it has it, and the port
// gets its flows only when the Connection knows the pod's node: the node is published as soon as the sandbox is up, not at Running.
func TestDevicePlacementIsPublishedWhileTheInitContainerRuns(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	dev := deviceOf("")
	dev.Namespace, dev.Spec.LabRef, dev.Spec.Name = "ns", "lab", "client"
	pick := func(pods ...*corev1.Pod) (string, string, string) {
		b := fake.NewClientBuilder().WithScheme(scheme)
		for _, p := range pods {
			b = b.WithObjects(p)
		}
		return (&DeviceReconciler{Client: b.Build()}).devicePodPlacement(context.Background(), dev)
	}
	if node, ip, name := pick(placementPod("p1", corev1.PodPending, "10.0.0.5", true)); node != "worker" || ip != "10.0.0.5" || name != "p1" {
		t.Errorf("a pod in its init container: %q %q %q", node, ip, name)
	}
	if node, _, _ := pick(placementPod("p1", corev1.PodPending, "", true)); node != "" {
		t.Errorf("no sandbox yet (no pod IP): no placement, got %q", node)
	}
	if node, _, _ := pick(placementPod("p1", corev1.PodPending, "10.0.0.5", false)); node != "" {
		t.Errorf("a Pending pod with no init container is not starting with a network: %q", node)
	}
	// during a replacement the Running pod wins over a starting one, whatever the order
	for _, order := range [][]*corev1.Pod{
		{placementPod("old", corev1.PodRunning, "10.0.0.1", false), placementPod("new", corev1.PodPending, "10.0.0.2", true)},
		{placementPod("new", corev1.PodPending, "10.0.0.2", true), placementPod("old", corev1.PodRunning, "10.0.0.1", false)},
	} {
		if _, _, name := pick(order...); name != "old" {
			t.Errorf("the Running pod must win, got %q", name)
		}
	}
}
