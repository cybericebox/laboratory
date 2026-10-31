package laboratory

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// A Lab deleted in a suspended group: the VPN pod is gone, and nobody is left to strip the finalizer the VPN binary puts on the LabVPN. The
// operator strips it when the pod is certainly gone (Deployment missing or at zero, no pod), and only then.
func TestLabVPNDeletionDoesNotWaitForAPodThatIsGone(t *testing.T) {
	zero, one := int32(0), int32(1)
	for _, tc := range []struct {
		name     string
		objects  func() []client.Object
		released bool
	}{
		{"deployment scaled to zero, no pod", func() []client.Object { return []client.Object{vpnDeployment(&zero)} }, true},
		{"no deployment, no pod", func() []client.Object { return nil }, true},
		{"deployment running", func() []client.Object { return []client.Object{vpnDeployment(&one)} }, false},
		{"scaled to zero but a pod is still there", func() []client.Object {
			return []client.Object{vpnDeployment(&zero), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vpn-x", Namespace: "g", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(s)
			_ = laboratoryv1alpha1.AddToScheme(s)
			parent := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g"}}
			parent.Spec.VPN.Enabled = true
			vpn := &laboratoryv1alpha1.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: names.LabVPNObjectName("a"), Namespace: "g", Finalizers: []string{names.FinalizerController, names.FinalizerVPN}}}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(tc.objects(), parent, vpn)...).Build()
			r := &LabReconciler{Client: c, Reader: c, Scheme: s}
			ctx := context.Background()
			if err := c.Delete(ctx, vpn); err != nil { // sets the deletion time (the finalizers hold it)
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if _, err := r.ensureLabVPNDeleted(ctx, parent); err != nil {
					t.Fatal(err)
				}
			}
			var got laboratoryv1alpha1.LabVPN
			err := c.Get(ctx, client.ObjectKeyFromObject(vpn), &got)
			gone := err != nil // all finalizers stripped: the object is deleted
			if gone != tc.released {
				t.Fatalf("released = %v, want %v (%v %v)", gone, tc.released, got.Finalizers, err)
			}
		})
	}
}

func vpnDeployment(replicas *int32) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: names.ComponentVPN, Namespace: "g"}, Spec: appsv1.DeploymentSpec{Replicas: replicas}}
}
