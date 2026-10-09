package devicestate

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func kubeRig(t *testing.T, dev *laboratoryv1alpha1.Device, pods ...*corev1.Pod) (*KubeCluster, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(dev).WithStatusSubresource(&laboratoryv1alpha1.Device{}).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		})
	for _, p := range pods {
		b = b.WithObjects(p)
	}
	c := b.Build()
	return &KubeCluster{Client: c, Reader: c, NodeName: "node-a"}, c
}

func stateDevice() *laboratoryv1alpha1.Device {
	return &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-web", Namespace: "ns"},
		Spec: laboratoryv1alpha1.DeviceSpec{
			Name: "web", LabRef: "lab",
			State: &laboratoryv1alpha1.DeviceStateSpec{
				Enabled: true, Debounce: metav1.Duration{Duration: 7 * time.Second},
				ExcludePaths: []string{"/cache"}, WriteQuotaBytes: 1000, MaxLayers: 4,
			},
		},
		Status: laboratoryv1alpha1.DeviceStatus{State: &laboratoryv1alpha1.DeviceStateStatus{Epoch: 2, Incarnation: 3}},
	}
}

func statePod(name, node string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Annotations: map[string]string{
			names.AnnotationStateDevice: "lab-web", names.AnnotationStateEpoch: "2", names.AnnotationStateIncarnation: "3",
		}},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "web", ContainerID: "containerd://abc123"},
		}},
	}
}

func TestKubeClusterListsOnlyLocalStatePods(t *testing.T) {
	other := statePod("elsewhere", "node-b", corev1.PodRunning)
	plain := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "ns"}, Spec: corev1.PodSpec{NodeName: "node-a"}}
	k, _ := kubeRig(t, stateDevice(), statePod("lab-web-3", "node-a", corev1.PodRunning), other, plain)
	pods, err := k.Pods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 {
		t.Fatalf("got %d pods: %+v", len(pods), pods)
	}
	p := pods[0]
	if p.Pod != "lab-web-3" || p.ContainerID != "abc123" || !p.Running || p.Ended || p.Epoch != 2 || p.DeviceEpoch != 2 || p.Incarnation != 3 {
		t.Fatalf("unexpected %+v", p)
	}
	if p.Repo != "lab/ns/lab/web" || p.Policy.Debounce != 7*time.Second || p.Policy.WriteQuota != 1000 || p.Policy.MaxLayers != 4 || !p.Policy.Excluded("/cache/x") {
		t.Fatalf("policy not taken from the device: %+v", p)
	}
}

func TestKubeClusterRecordWarnAndMarkExit(t *testing.T) {
	k, c := kubeRig(t, stateDevice())
	ctx := context.Background()
	p := PodInfo{Device: types.NamespacedName{Namespace: "ns", Name: "lab-web"}, Pod: "lab-web-3", Epoch: 2, Incarnation: 3}
	get := func() *laboratoryv1alpha1.DeviceStateStatus {
		var d laboratoryv1alpha1.Device
		if err := c.Get(ctx, p.Device, &d); err != nil {
			t.Fatal(err)
		}
		return d.Status.State
	}

	if err := k.Warn(ctx, p, "quota"); err != nil || get().Warning != "quota" {
		t.Fatalf("warn: %v %+v", err, get())
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := k.Record(ctx, p, Snapshot{Image: "localhost:5035/x@sha256:ab", At: at, SizeBytes: 42, Layers: 2}); err != nil {
		t.Fatal(err)
	}
	st := get()
	if st.Image != "localhost:5035/x@sha256:ab" || st.SizeBytes != 42 || st.Layers != 2 || st.Warning != "" || st.SnapshotAt == nil || !st.SnapshotAt.Time.Equal(at) {
		t.Fatalf("record not applied: %+v", st)
	}
	if st.Epoch != 2 || st.Incarnation != 3 {
		t.Fatal("the node-agent must not touch the controller's fields")
	}
	// Recording the base state clears the image.
	if err := k.Record(ctx, p, Snapshot{At: at}); err != nil || get().Image != "" {
		t.Fatalf("base record: %v %+v", err, get())
	}
	if err := k.MarkExit(ctx, p); err != nil || get().ExitSnapshotPod != "lab-web-3" {
		t.Fatalf("mark exit: %v %+v", err, get())
	}
}

func TestKubeClusterDropsWritesOfStalePods(t *testing.T) {
	k, c := kubeRig(t, stateDevice())
	ctx := context.Background()
	dev := types.NamespacedName{Namespace: "ns", Name: "lab-web"}

	oldEpoch := PodInfo{Device: dev, Pod: "lab-web-2", Epoch: 1, Incarnation: 3}
	if err := k.Record(ctx, oldEpoch, Snapshot{Image: "x"}); !errors.Is(err, ErrStale) {
		t.Fatalf("a pod from before a reset must not record, got %v", err)
	}
	if err := k.MarkExit(ctx, oldEpoch); !errors.Is(err, ErrStale) {
		t.Fatalf("got %v", err)
	}
	oldIncarnation := PodInfo{Device: dev, Pod: "lab-web-2", Epoch: 2, Incarnation: 2}
	if err := k.Record(ctx, oldIncarnation, Snapshot{Image: "x"}); !errors.Is(err, ErrStale) {
		t.Fatalf("a superseded incarnation must not record its snapshot, got %v", err)
	}
	// Its exit marker is still accepted: the controller is waiting for exactly that.
	if err := k.MarkExit(ctx, oldIncarnation); err != nil {
		t.Fatal(err)
	}
	var d laboratoryv1alpha1.Device
	if err := c.Get(ctx, dev, &d); err != nil {
		t.Fatal(err)
	}
	if d.Status.State.Image != "" {
		t.Fatal("stale write leaked into the status")
	}
}
