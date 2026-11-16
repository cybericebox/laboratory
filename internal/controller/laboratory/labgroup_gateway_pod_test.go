package laboratory

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// gatewayPodFixture is a group with its reconciler on a fake client; labs are added and removed by the tests.
func gatewayPodFixture(t *testing.T, scheduled bool) (*LabGroupReconciler, client.Client, *api.LabGroup) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	group := &api.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "gw-follows-labs"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group).WithStatusSubresource(&api.LabGroup{}, &allocation.Pool{}, &appsv1.Deployment{}).Build()
	r := &LabGroupReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(1000), LabBaseNetwork: "10.128.0.0/10",
		VPNImage: "lab:v1", GatewayImage: "lab:v1", SupportEmail: "help@example.com", Scheduled: scheduled}
	return r, c, group
}

func reconcileGroup(t *testing.T, r *LabGroupReconciler, g *api.LabGroup) api.LabGroup {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(g)}); err != nil {
		t.Fatal(err)
	}
	var got api.LabGroup
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(g), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func hasDeployment(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	var d appsv1.Deployment
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &d)
	if errors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func gatewayLab(ns, name string, kinds ...api.GatewayKind) *api.Lab {
	lab := &api.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	for i, k := range kinds {
		lab.Spec.Gateways = append(lab.Spec.Gateways, api.GatewayTemplate{Name: string(k) + string(rune('a'+i)), Kind: k})
	}
	return lab
}

// The gateway pod exists only while some lab of the group has an Internet gateway: made when the first one is added to a running group,
// removed with the last. The VPN pod is not counted this way (it follows spec.vpn.disabled, which the platform sets).
func TestGatewayPodFollowsTheLabsOfTheGroup(t *testing.T) {
	ctx := context.Background()
	r, c, group := gatewayPodFixture(t, false)
	ns := api.LabGroupNamespace(group.Name)

	got := reconcileGroup(t, r, group)
	if got.Status.GatewayPod == nil || *got.Status.GatewayPod {
		t.Fatalf("a group with no lab needs no gateway pod: %v", got.Status.GatewayPod)
	}
	if hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Error("no lab: no gateway Deployment")
	}
	if !hasDeployment(t, c, ns, names.ComponentVPN) {
		t.Error("the VPN pod does not follow the labs")
	}

	// a lab with only a VPN gateway, and one with none, do not need it either
	for _, lab := range []*api.Lab{gatewayLab(ns, "vpn-only", api.GatewayKindVPN), gatewayLab(ns, "bare")} {
		if err := c.Create(ctx, lab); err != nil {
			t.Fatal(err)
		}
	}
	got = reconcileGroup(t, r, group)
	if *got.Status.GatewayPod || hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Fatal("a VPN gateway or no gateway needs no gateway pod")
	}

	// a lab with an Internet gateway is added to the running group: the pod is made
	net := gatewayLab(ns, "internet", api.GatewayKindInternet)
	if err := c.Create(ctx, net); err != nil {
		t.Fatal(err)
	}
	got = reconcileGroup(t, r, group)
	if !*got.Status.GatewayPod || !hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Fatal("an Internet gateway needs the gateway pod")
	}
	var sa appsv1.Deployment
	_ = c.Get(ctx, client.ObjectKey{Namespace: ns, Name: names.ComponentGateway}, &sa)
	if sa.Spec.Replicas == nil || *sa.Spec.Replicas != 1 {
		t.Errorf("the gateway pod runs one replica: %v", sa.Spec.Replicas)
	}

	// a second one changes nothing; the first one going leaves the pod to the second
	second := gatewayLab(ns, "internet-2", api.GatewayKindInternet)
	if err := c.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	reconcileGroup(t, r, group)
	if err := c.Delete(ctx, net); err != nil {
		t.Fatal(err)
	}
	if got = reconcileGroup(t, r, group); !*got.Status.GatewayPod || !hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Fatal("another lab still needs the gateway pod")
	}

	// the last one goes: the pod goes with it
	if err := c.Delete(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got = reconcileGroup(t, r, group); *got.Status.GatewayPod || hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Fatal("no lab with an Internet gateway is left: the gateway pod must be removed")
	}
	if !hasDeployment(t, c, ns, names.ComponentVPN) {
		t.Error("the VPN pod stays")
	}
}

// A lab that is being deleted still counts: its LabGateway is removed by the pod, so the pod waits until the lab is gone.
func TestGatewayPodStaysWhileATerminatingLabHasItsGateway(t *testing.T) {
	ctx := context.Background()
	r, c, group := gatewayPodFixture(t, false)
	ns := api.LabGroupNamespace(group.Name)
	lab := gatewayLab(ns, "going", api.GatewayKindInternet)
	lab.Finalizers = []string{names.FinalizerController}
	if err := c.Create(ctx, lab); err != nil {
		t.Fatal(err)
	}
	reconcileGroup(t, r, group)
	if err := c.Delete(ctx, lab); err != nil { // finalizer: the Lab stays, with a deletion time
		t.Fatal(err)
	}
	if got := reconcileGroup(t, r, group); !*got.Status.GatewayPod || !hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Fatal("the gateway pod must stay while the lab that has its gateway still exists")
	}
	var gone api.Lab
	if err := c.Get(ctx, client.ObjectKeyFromObject(lab), &gone); err != nil {
		t.Fatal(err)
	}
	gone.Finalizers = nil
	if err := c.Update(ctx, &gone); err != nil {
		t.Fatal(err)
	}
	if got := reconcileGroup(t, r, group); *got.Status.GatewayPod || hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Fatal("the lab is gone: so is the gateway pod")
	}
}

// With the scheduler on, a gateway pod wanted again is queued afresh (its old record is dropped when it is not wanted), so it does not
// start ahead of the scheduler.
func TestGatewayPodIsQueuedAgainWhenWantedAgain(t *testing.T) {
	ctx := context.Background()
	r, c, group := gatewayPodFixture(t, true)
	ns := api.LabGroupNamespace(group.Name)
	record := func(g api.LabGroup, name string) *api.NamedPodSchedule {
		for i := range g.Status.Pods {
			if g.Status.Pods[i].Name == name {
				return &g.Status.Pods[i]
			}
		}
		return nil
	}

	got := reconcileGroup(t, r, group)
	if record(got, names.ComponentGateway) != nil || record(got, names.ComponentVPN) == nil {
		t.Fatalf("only the VPN pod is queued with no lab: %+v", got.Status.Pods)
	}
	lab := gatewayLab(ns, "internet", api.GatewayKindInternet)
	if err := c.Create(ctx, lab); err != nil {
		t.Fatal(err)
	}
	got = reconcileGroup(t, r, group)
	if rec := record(got, names.ComponentGateway); rec == nil || rec.State != api.PodQueued {
		t.Fatalf("the new gateway pod waits for the scheduler: %+v", got.Status.Pods)
	}
	if hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Fatal("a queued gateway pod has no Deployment yet")
	}

	// the scheduler lets it start
	got.Status.Pods = append(got.Status.Pods[:0:0], got.Status.Pods...)
	for i := range got.Status.Pods {
		got.Status.Pods[i].State = api.PodStarted
	}
	if err := c.Status().Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	reconcileGroup(t, r, group)
	if !hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Fatal("a started gateway pod has its Deployment")
	}

	// the lab goes: the pod and its record go; a lab again: queued again, not started
	if err := c.Delete(ctx, lab); err != nil {
		t.Fatal(err)
	}
	got = reconcileGroup(t, r, group)
	if hasDeployment(t, c, ns, names.ComponentGateway) || record(got, names.ComponentGateway) != nil {
		t.Fatalf("pod and record must go with the last lab: %+v", got.Status.Pods)
	}
	if err := c.Create(ctx, gatewayLab(ns, "internet-again", api.GatewayKindInternet)); err != nil {
		t.Fatal(err)
	}
	got = reconcileGroup(t, r, group)
	if rec := record(got, names.ComponentGateway); rec == nil || rec.State != api.PodQueued || hasDeployment(t, c, ns, names.ComponentGateway) {
		t.Fatalf("wanted again: queued afresh, no Deployment: %+v", got.Status.Pods)
	}
}

// A lab of the group's namespace re-reconciles the group; a namespace that is no group's does not.
func TestLabMapsToTheGroupOfItsNamespace(t *testing.T) {
	r, c, group := gatewayPodFixture(t, false)
	reconcileGroup(t, r, group) // makes the namespace with its group label
	ns := api.LabGroupNamespace(group.Name)
	reqs := r.groupOfLab(context.Background(), gatewayLab(ns, "x"))
	if len(reqs) != 1 || reqs[0].Name != group.Name {
		t.Errorf("a lab of the group's namespace maps to the group: %v", reqs)
	}
	if reqs := r.groupOfLab(context.Background(), gatewayLab("elsewhere", "x")); len(reqs) != 0 {
		t.Errorf("a lab outside any group namespace maps to nothing: %v", reqs)
	}
	_ = c
}
