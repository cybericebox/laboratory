//go:build linux

package gateway

import (
	"context"
	"errors"
	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

type failLabRead struct{ client.Client }

func (c failLabRead) Get(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*lab.Lab); ok {
		return errors.New("forbidden Labs read")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func TestGatewayReadFailureClearsDHCPReadiness(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	gw := &lab.LabGateway{ObjectMeta: metav1.ObjectMeta{Name: "a-gw", Namespace: "g", Finalizers: []string{names.FinalizerController, names.FinalizerGateway}}, Spec: lab.LabGatewaySpec{LabName: "a", NetworkIndex: 1}, Status: lab.LabGatewayStatus{Phase: lab.LabGatewayPhaseReady, NATReady: true, DHCPReady: true, DHCPEnabled: true}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gw).WithObjects(gw).Build()
	m := dhcp.NewManager()
	defer m.Close()
	r := &LabGatewayReconciler{Client: failLabRead{c}, DHCP: m}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)}); err == nil {
		t.Fatal("read error swallowed")
	}
	var got lab.LabGateway
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.DHCPReady || got.Status.Phase != lab.LabGatewayPhaseConfiguring || !got.Status.NATReady {
		t.Fatalf("stale/misleading readiness %+v", got.Status)
	}
}

func TestGatewayMissingInterfaceCannotKeepReady(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	gw := &lab.LabGateway{ObjectMeta: metav1.ObjectMeta{Name: "a-gw", Namespace: "g", Finalizers: []string{names.FinalizerController, names.FinalizerGateway}}, Spec: lab.LabGatewaySpec{LabName: "a", NetworkIndex: 65000}, Status: lab.LabGatewayStatus{Phase: lab.LabGatewayPhaseReady, NATReady: true, DHCPReady: true}}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(gw).WithObjects(gw, l).Build()
	m := dhcp.NewManager()
	defer m.Close()
	r := &LabGatewayReconciler{Client: c, DHCP: m}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)}); err != nil {
		t.Fatal(err)
	}
	var got lab.LabGateway
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(gw), &got)
	if got.Status.DHCPReady || got.Status.NATReady || got.Status.Phase == lab.LabGatewayPhaseReady {
		t.Fatalf("missing interface still Ready %+v", got.Status)
	}
}
