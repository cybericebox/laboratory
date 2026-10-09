package laboratory

import (
	"context"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func residualGroupStartFixture(t *testing.T, firstAlreadyScaled bool) (*LabGroupReconciler, *lab.LabGroup, client.Client) {
	t.Helper()
	scheme := retentionScheme(t)
	_ = corev1.AddToScheme(scheme)
	g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", UID: "group", Generation: 3}, Spec: lab.LabGroupSpec{Lifecycle: &lab.GroupLifecycleSpec{DesiredState: "Running", OperationID: "start2", Revision: 2}}, Status: lab.LabGroupStatus{Namespace: "ns"}}
	objects := []client.Object{g, groupStoppedChild()}
	controller := true
	for _, component := range []string{names.ComponentVPN, names.ComponentGateway} {
		replicas := int32(1)
		if firstAlreadyScaled && component == names.ComponentVPN {
			replicas = 0
		}
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component, Namespace: "ns", UID: types.UID("dep-" + component)}, Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: component, Env: []corev1.EnvVar{{Name: "GROUP_UID", Value: "group"}}}}}}}}
		rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: component + "-rs", Namespace: "ns", UID: types.UID(component + "-rs"), OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: component, UID: dep.UID, Controller: &controller}}}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: component + "-old", Namespace: "ns", UID: types.UID(component + "-old"), Labels: map[string]string{names.LabelComponent: component}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &controller}}}}
		objects = append(objects, dep, rs, pod)
		g.Status.ServiceRuntime = append(g.Status.ServiceRuntime, lab.OwnedRuntimeIdentity{OwnerUID: "group", OperationID: "stop1", Revision: 1, DeploymentUID: string(dep.UID), Component: component, PodUID: string(pod.UID), NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"actual-" + component}, CgroupPaths: []string{"/owned/" + component}, AttachmentsComplete: true})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(g, &appsv1.Deployment{}).WithObjects(objects...).Build()
	return &LabGroupReconciler{Client: c, Reader: c}, g, c
}
func TestResidualGroupStartActivelyDrainsOriginalInventoryBeforeProvisioning(t *testing.T) {
	for _, between := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-first-scale", true: "between-scales"}[between], func(t *testing.T) {
			r, g, c := residualGroupStartFixture(t, between)
			ctx := context.Background()
			handled, _, err := r.reconcileGroupLifecycle(ctx, g)
			if err != nil || !handled {
				t.Fatalf("old inventory not actively drained: %v %v", handled, err)
			}
			for _, component := range []string{names.ComponentVPN, names.ComponentGateway} {
				var dep appsv1.Deployment
				if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: component}, &dep); err != nil || *dep.Spec.Replicas != 0 {
					t.Fatalf("original service remained live: %s %v", component, err)
				}
			}
			var pods corev1.PodList
			_ = c.List(ctx, &pods, client.InNamespace("ns"))
			if len(pods.Items) != 0 {
				t.Fatal("exact original Pods did not drain")
			}
			var child lab.Lab
			_ = c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "child"}, &child)
			if !child.Spec.Lifecycle.IsStopped() {
				t.Fatal("group drain revived stopped child")
			}
			// Native release is deliberately not manufactured here: this verifies the
			// active producer boundary, while the Linux adapter/application proves ACKs.
			_ = c.Get(ctx, client.ObjectKeyFromObject(g), g)
			if g.Status.Lifecycle.ObservedState != "Starting" {
				t.Fatal("API teardown claimed current Running before native ACK")
			}
		})
	}
}
func TestResidualGroupDrainRefusesReplacementDeployment(t *testing.T) {
	r, g, c := residualGroupStartFixture(t, false)
	ctx := context.Background()
	var dep appsv1.Deployment
	_ = c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: names.ComponentVPN}, &dep)
	dep.UID = "replacement"
	_ = c.Update(ctx, &dep)
	if err := r.drainPriorGroupServices(ctx, g); err == nil {
		t.Fatal("replacement Deployment was accepted for old teardown")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(&dep), &dep)
	if *dep.Spec.Replicas != 1 {
		t.Fatal("replacement service was scaled down")
	}
}
func TestResidualGroupDrainRefusesReplacementPodAndGroupMarker(t *testing.T) {
	for _, scenario := range []string{"replacement-pod", "foreign-group"} {
		t.Run(scenario, func(t *testing.T) {
			r, g, c := residualGroupStartFixture(t, false)
			ctx := context.Background()
			var dep appsv1.Deployment
			_ = c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: names.ComponentVPN}, &dep)
			if scenario == "replacement-pod" {
				var p corev1.Pod
				_ = c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: names.ComponentVPN + "-old"}, &p)
				p.UID = "replacement"
				_ = c.Update(ctx, &p)
			} else {
				dep.Spec.Template.Spec.Containers[0].Env[0].Value = "other-group"
				_ = c.Update(ctx, &dep)
			}
			if err := r.drainPriorGroupServices(ctx, g); err == nil {
				t.Fatal("replacement authority accepted")
			}
			_ = c.Get(ctx, client.ObjectKeyFromObject(&dep), &dep)
			if *dep.Spec.Replicas != 1 {
				t.Fatal("replacement/foreign service scaled down")
			}
		})
	}
}
