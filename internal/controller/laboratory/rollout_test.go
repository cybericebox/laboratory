package laboratory

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
)

func TestPullPolicyIsAlwaysIfNotPresent(t *testing.T) {
	for _, image := range []string{
		"cybericebox/laboratory-lab:v1.2.3",
		"localhost:5035/x/lab@sha256:abcdef",
		"registry.example.com:5000/lab:1.0",
		"localhost:5035/docker.io/x/lab:v2@sha256",
	} {
		if got := pullPolicyFor(image); got != corev1.PullIfNotPresent {
			t.Errorf("%s: %s, want %s", image, got, corev1.PullIfNotPresent)
		}
	}
}

// B-3: the configured image reaches existing VPN and gateway Deployments, one group at a time.
func TestImageUpdateRollsOneGroupAtATime(t *testing.T) {
	ctx := context.Background()
	scheme := clientgoscheme.Scheme
	dep := func(ns, name, image string) *appsv1.Deployment {
		res := grouppods.Config{}.VPN()
		if name == "gateway" {
			res = grouppods.Config{}.Gateway()
		}
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptrInt32(1),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: image, ImagePullPolicy: corev1.PullIfNotPresent, Resources: res, Command: []string{"/lab", name}}}},
				},
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		dep("ns-a", "vpn", "lab:v1"), dep("ns-a", "gateway", "lab:v1"),
		dep("ns-b", "vpn", "lab:v1"), dep("ns-b", "gateway", "lab:v1"),
		dep("ns-c", "vpn", "lab:v2"), dep("ns-c", "gateway", "lab:v2"), // already current
	).Build()
	r := &LabGroupReconciler{Client: c, VPNBaseNetwork: "10.8.0.0/10", VPNImage: "lab:v2", GatewayImage: "lab:v2", GroupPods: grouppods.Config{}}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	r.rollout.now = func() time.Time { return now }

	image := func(ns, name string) string {
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &d); err != nil {
			t.Fatal(err)
		}
		return d.Spec.Template.Spec.Containers[0].Image
	}
	ensure := func(ns string) {
		t.Helper()
		r.rollout.begin(ns)
		if err := r.ensureVPNDeployment(ctx, ns, false, nil); err != nil {
			t.Fatal(err)
		}
		if err := r.ensureGatewayDeployment(ctx, ns, false, nil); err != nil {
			t.Fatal(err)
		}
	}

	ensure("ns-a")
	if image("ns-a", "vpn") != "lab:v2" || image("ns-a", "gateway") != "lab:v2" {
		t.Fatal("the first group takes the update at once, VPN and gateway together")
	}
	ensure("ns-b")
	if image("ns-b", "vpn") != "lab:v1" {
		t.Fatal("a second group must wait for the first")
	}
	if again, err := r.settleRollout(ctx, "ns-b"); err != nil || !again {
		t.Fatalf("the waiting group must be looked at again: %v %v", again, err)
	}
	if again, err := r.settleRollout(ctx, "ns-a"); err != nil || !again {
		t.Fatalf("the first group is still rolling (its pods are not ready): %v %v", again, err)
	}
	// the pods of group a are up
	for _, n := range []string{"vpn", "gateway"} {
		var d appsv1.Deployment
		_ = c.Get(ctx, types.NamespacedName{Name: n, Namespace: "ns-a"}, &d)
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
		if err := c.Status().Update(ctx, &d); err != nil {
			t.Fatal(err)
		}
	}
	if again, err := r.settleRollout(ctx, "ns-a"); err != nil || again {
		t.Fatalf("the rolled-out group releases its turn: %v %v", again, err)
	}
	ensure("ns-b")
	if image("ns-b", "vpn") != "lab:v2" || image("ns-b", "gateway") != "lab:v2" {
		t.Fatal("the second group takes its turn")
	}
	// a group that is current never takes a turn
	ensure("ns-c")
	if r.rollout.isWaiting("ns-c") {
		t.Fatal("a current group does not wait")
	}
}

// B-3: a group whose new pods never become ready does not hold the turn forever.
func TestRolloutTurnGoesStale(t *testing.T) {
	var g rolloutGuard
	now := time.Unix(1000, 0)
	g.now = func() time.Time { return now }
	if !g.try("a") || g.try("b") {
		t.Fatal("a takes the turn, b waits")
	}
	now = now.Add(rolloutStale + time.Second)
	if !g.try("b") {
		t.Fatal("a stale turn passes on")
	}
}

// B-3: an upgrade converges the image and the pull policy of an existing VPN or gateway Deployment and nothing else: the size of the VPN
// (and the command) stay, whatever the chart says now. A gateway that is rolled for the image takes the size of the day in the same roll.
func TestImageUpdateKeepsTheSizeOfAGroup(t *testing.T) {
	ctx := context.Background()
	small := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("37m"), corev1.ResourceMemory: resource.MustParse("77Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("37m"), corev1.ResourceMemory: resource.MustParse("77Mi")},
	}
	dep := func(name string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Generation: 1},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptrInt32(1),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: name, Image: "lab:v1", ImagePullPolicy: corev1.PullAlways, Resources: small, Command: []string{"/lab", name, "--old"},
					}}},
				},
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(dep("vpn"), dep("gateway")).Build()
	r := &LabGroupReconciler{Client: c, VPNBaseNetwork: "10.8.0.0/10", VPNImage: "lab:v2", GatewayImage: "lab:v2",
		GroupPods: grouppods.Config{VPNCPU: "500m", VPNMemory: "1Gi", GatewayCPU: "200m", GatewayMemory: "512Mi"}}
	if err := r.ensureVPNDeployment(ctx, "ns", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureGatewayDeployment(ctx, "ns", false, nil); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"vpn", "gateway"} {
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{Name: n, Namespace: "ns"}, &d); err != nil {
			t.Fatal(err)
		}
		ct := d.Spec.Template.Spec.Containers[0]
		if ct.Image != "lab:v2" || ct.ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("%s: image %s, policy %s: the image and the pull policy must converge", n, ct.Image, ct.ImagePullPolicy)
		}
		want := small
		if n == "gateway" {
			want = r.GroupPods.GatewayFor(nil)
		}
		if !apiequality.Semantic.DeepEqual(ct.Resources, want) {
			t.Errorf("%s: resources %v, want %v", n, ct.Resources, want)
		}
		if len(ct.Command) != 3 || ct.Command[2] != "--old" {
			t.Errorf("%s: command %v: it must not be touched", n, ct.Command)
		}
	}
}

// A pure change of the size in the chart restarts no running gateway; one with a size of its own keeps it when it is rolled.
func TestGatewaySizeFollowsTheChartOnlyWithARoll(t *testing.T) {
	ctx := context.Background()
	old := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
	}
	dep := func(ns, image string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: ns, Generation: 1},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptrInt32(1),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "gateway", "laboratory.cybericebox.com/component": "gateway"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "gateway", Image: image, ImagePullPolicy: corev1.PullIfNotPresent, Resources: old, Command: []string{"/lab", "gateway"},
					}}},
				},
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(dep("a", "lab:v2"), dep("b", "lab:v2")).Build()
	r := &LabGroupReconciler{Client: c, GatewayImage: "lab:v2", GroupPods: grouppods.Config{GatewayCPU: "100m", GatewayMemory: "96Mi"}}
	own := &laboratoryv1alpha1.GroupPodSize{CPUMillicores: 55, MemoryBytes: 64 << 20}
	ensure := func(ns string, size *laboratoryv1alpha1.GroupPodSize) {
		t.Helper()
		r.rollout.begin(ns)
		r.rollout.release(ns)
		if err := r.ensureGatewayDeployment(ctx, ns, false, size); err != nil {
			t.Fatal(err)
		}
		r.rollout.release(ns) // the roll is done: the next group may have its turn
	}
	resources := func(ns string) corev1.ResourceRequirements {
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{Name: "gateway", Namespace: ns}, &d); err != nil {
			t.Fatal(err)
		}
		return d.Spec.Template.Spec.Containers[0].Resources
	}
	setOld := func(ns string) {
		t.Helper()
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{Name: "gateway", Namespace: ns}, &d); err != nil {
			t.Fatal(err)
		}
		d.Spec.Template.Spec.Containers[0].Resources = old
		if err := c.Update(ctx, &d); err != nil {
			t.Fatal(err)
		}
	}
	// the first pass brings the fixtures to the template of the day (they are rolled once)
	ensure("a", nil)
	ensure("b", own)
	setOld("a")
	setOld("b")

	ensure("a", nil)
	if !apiequality.Semantic.DeepEqual(resources("a"), old) {
		t.Errorf("a gateway that is not rolled keeps its size: %v", resources("a"))
	}
	// the image changes: both groups are rolled
	r.GatewayImage = "lab:v3"
	ensure("a", nil)
	ensure("b", own)
	if !apiequality.Semantic.DeepEqual(resources("a"), r.GroupPods.Gateway()) {
		t.Errorf("a gateway that is rolled takes the size of the day: %v", resources("a"))
	}
	if !apiequality.Semantic.DeepEqual(resources("b"), r.GroupPods.GatewayFor(own)) {
		t.Errorf("a gateway with a size of its own is rolled to that size: %v", resources("b"))
	}
}
