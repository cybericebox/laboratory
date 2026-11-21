package nodeagent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// An endpoint of a connection is a gateway when the lab declares one under that name; its kind says which pod serves it.
func TestGatewayOfNamesTheGatewaysOfTheLab(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	l := &laboratoryv1alpha1.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g"},
		Spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{
			{Name: "attack", Kind: laboratoryv1alpha1.GatewayKindVPN}, {Name: "out", Kind: laboratoryv1alpha1.GatewayKindInternet},
		}},
	}
	r := &ConnectionReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(l).Build()}
	conn := &laboratoryv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Namespace: "g"}, Spec: laboratoryv1alpha1.ConnectionSpec{LabRef: "a"}}
	for name, want := range map[string]struct {
		kind laboratoryv1alpha1.GatewayKind
		ok   bool
	}{
		"attack": {laboratoryv1alpha1.GatewayKindVPN, true}, "out": {laboratoryv1alpha1.GatewayKindInternet, true},
		"vpn": {"", false}, "web": {"", false},
	} {
		kind, ok, err := r.gatewayOf(context.Background(), conn, name)
		if err != nil || ok != want.ok || kind != want.kind {
			t.Errorf("%s: %q %v %v, want %+v", name, kind, ok, err, want)
		}
	}
	conn.Spec.LabRef = "gone"
	if _, ok, err := r.gatewayOf(context.Background(), conn, "attack"); err != nil || ok {
		t.Errorf("a lab that is gone has no gateways: %v %v", ok, err)
	}
}
