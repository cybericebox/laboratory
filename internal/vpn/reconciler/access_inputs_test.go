//go:build linux

package reconciler

import (
	"context"
	"errors"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vpn"
)

// The double models the external kernel side effect, including a replacement
// that clears the old chain and then fails. The reconciler/client stay real.
type recordingAccessApplier struct {
	applications int
	active       []vpn.AccessRule
	failNext     bool
}

func (a *recordingAccessApplier) ReplaceAccessRules(rules []vpn.AccessRule) error {
	a.applications++
	a.active = nil
	if a.failNext {
		a.failNext = false
		return errors.New("kernel replacement failed after clearing the chain")
	}
	a.active = slices.Clone(rules)
	return nil
}

func (*recordingAccessApplier) AccessCounters() (map[string]vpn.TrafficCounter, error) {
	return map[string]vpn.TrafficCounter{}, nil
}

type recordingAccessRevoker struct {
	calls    int
	failNext bool
}

func (r *recordingAccessRevoker) Revoke([]string, []vpn.AccessRule) (int, error) {
	r.calls++
	if r.failNext {
		r.failNext = false
		return 0, errors.New("conntrack unavailable")
	}
	return 0, nil
}

func accessReconcileFixture(t *testing.T) (*AccessReconciler, *recordingAccessApplier, client.Client, ctrl.Request) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := lab.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	peer := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "group"}, Status: lab.LabGroupClientStatus{AssignedIP: "10.8.0.2/32"}}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: "group"}, Status: lab.LabStatus{Phase: lab.PhaseReady, VPN: lab.LabNetworkStatus{Ready: true, CIDR: "10.8.1.0/24"}}}
	p := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "group", Generation: 1}, Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(peer, l, p).WithObjects(peer, l, p).Build()
	a := &recordingAccessApplier{}
	return &AccessReconciler{Client: c, IPT: a}, a, c, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "group", Name: names.LabGroupAccessPolicyName}}
}

func reconcileAccess(t *testing.T, r *AccessReconciler, req ctrl.Request) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func changePeer(t *testing.T, c client.Client, update func(*lab.LabGroupClient)) {
	t.Helper()
	var p lab.LabGroupClient
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "group", Name: "p1"}, &p); err != nil {
		t.Fatal(err)
	}
	update(&p)
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
}

func TestAccessReconcileTelemetryDoesNotReapplyOrRevoke(t *testing.T) {
	r, a, c, req := accessReconcileFixture(t)
	revoker := &recordingAccessRevoker{}
	r.Conntrack = revoker
	reconcileAccess(t, r, req)
	for i := 0; i < 100; i++ {
		changePeer(t, c, func(p *lab.LabGroupClient) { p.Status.Statistics.RxBytes++ })
		reconcileAccess(t, r, req)
	}
	if a.applications != 1 || revoker.calls != 1 {
		t.Fatalf("unchanged permissions caused kernel work: apply=%d revoke=%d", a.applications, revoker.calls)
	}
	changePeer(t, c, func(p *lab.LabGroupClient) { p.Status.AssignedIP = "10.8.0.3/32" })
	reconcileAccess(t, r, req)
	if a.applications != 2 || revoker.calls != 2 || a.active[0].SourceCIDR != "10.8.0.3/32" {
		t.Fatalf("new allocation was not installed: %+v, revoke=%d", a, revoker.calls)
	}
}

func TestAccessReconcileFailedReplacementThenReversionRepairsKernel(t *testing.T) {
	r, a, c, req := accessReconcileFixture(t)
	reconcileAccess(t, r, req)
	initial := slices.Clone(a.active)
	a.failNext = true
	changePeer(t, c, func(p *lab.LabGroupClient) { p.Status.AssignedIP = "10.8.0.3/32" })
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("failed kernel replacement was reported successful")
	}
	if len(a.active) != 0 {
		t.Fatal("test dependency did not model the cleared kernel chain")
	}
	changePeer(t, c, func(p *lab.LabGroupClient) { p.Status.AssignedIP = "10.8.0.2/32" })
	reconcileAccess(t, r, req)
	if a.applications != 3 || !slices.Equal(a.active, initial) {
		t.Fatalf("reverted policy skipped repair of the partial kernel update: %+v", a)
	}
}

func TestAccessReconcileRevocationRetryDoesNotReapplyRules(t *testing.T) {
	r, a, _, req := accessReconcileFixture(t)
	revoker := &recordingAccessRevoker{failNext: true}
	r.Conntrack = revoker
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("revocation failure was hidden")
	}
	var status lab.LabGroupAccessPolicy
	if err := r.Client.Get(context.Background(), req.NamespacedName, &status); err != nil {
		t.Fatal(err)
	}
	if status.Status.State != "Failed" || status.Status.LastError == "" {
		t.Fatalf("revocation failure left a misleading status: %+v", status.Status)
	}
	reconcileAccess(t, r, req)
	if a.applications != 1 || revoker.calls != 2 {
		t.Fatalf("revocation retry repeated firewall work: apply=%d revoke=%d", a.applications, revoker.calls)
	}
}

func TestAccessReconcileColdStartAppliesEvenAfterPreviousReadyStatus(t *testing.T) {
	r, a, c, req := accessReconcileFixture(t)
	reconcileAccess(t, r, req)
	restarted := &AccessReconciler{Client: c, IPT: a}
	reconcileAccess(t, restarted, req)
	if a.applications != 2 {
		t.Fatal("new process trusted old API status without configuring the kernel")
	}
}
