//go:build linux

package reconciler

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

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
	calls       int
	failNext    bool
	lastClients []string
	lastRules   []vpn.AccessRule
}

func (r *recordingAccessRevoker) Revoke(_ []string, rules []vpn.AccessRule, clients ...[]string) (int, error) {
	r.calls++
	r.lastRules = slices.Clone(rules)
	if len(clients) != 0 {
		r.lastClients = slices.Clone(clients[0])
	}
	if r.failNext {
		r.failNext = false
		return 0, errors.New("conntrack unavailable")
	}
	return 0, nil
}

func TestAccessReconcileRevocationRetryKeepsRetiredAddress(t *testing.T) {
	r, _, c, req := accessReconcileFixture(t)
	revoker := &recordingAccessRevoker{}
	r.Conntrack = revoker
	reconcileAccess(t, r, req)
	changePeer(t, c, func(p *lab.LabGroupClient) { p.Status.AssignedIP = "10.8.0.3/32" })
	revoker.failNext = true
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("revocation failure was hidden")
	}
	reconcileAccess(t, r, req)
	if !slices.Contains(revoker.lastClients, "10.8.0.2/32") {
		t.Fatalf("retired address was lost on revocation retry: %v", revoker.lastClients)
	}
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(peer, l, p, &lab.LabVPN{}, &lab.LabTrafficReport{}).WithObjects(peer, l, p).Build()
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
	if err := r.Get(context.Background(), req.NamespacedName, &status); err != nil {
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

func TestAccessReconcileReissuedAddressRevokesOldOwnerFlows(t *testing.T) {
	r, _, c, req := accessReconcileFixture(t)
	revoker := &recordingAccessRevoker{}
	r.Conntrack = revoker
	reconcileAccess(t, r, req)
	old := &lab.LabGroupClient{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "group", Name: "p1"}, old)
	if err := c.Delete(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	next := old.DeepCopy()
	next.Name = "p2"
	next.ResourceVersion = ""
	next.UID = ""
	if err := c.Create(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	reconcileAccess(t, r, req)
	for _, rule := range revoker.lastRules {
		if rule.Action == vpn.AccessAllow && rule.SourceCIDR == "10.8.0.2/32" {
			t.Fatal("old established flows survived address reissue")
		}
	}
}

func reissuePeer(t *testing.T, c client.Client) {
	t.Helper()
	old := &lab.LabGroupClient{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "group", Name: "p1"}, old)
	if err := c.Delete(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	next := old.DeepCopy()
	next.Name = "p2"
	next.ResourceVersion = ""
	next.UID = ""
	if err := c.Create(context.Background(), next); err != nil {
		t.Fatal(err)
	}
}
func TestReissuedBindingStaysClosedUntilRevocationSucceeds(t *testing.T) {
	r, a, c, req := accessReconcileFixture(t)
	revoker := &recordingAccessRevoker{}
	r.Conntrack = revoker
	reconcileAccess(t, r, req)
	reissuePeer(t, c)
	revoker.failNext = true
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected retirement failure")
	}
	for _, rule := range a.active {
		if rule.ClientName == "p2" && rule.Action == vpn.AccessAllow {
			t.Fatal("new owner opened while old flows remain")
		}
	}
	reconcileAccess(t, r, req)
	if len(a.active) != 1 || a.active[0].Action != vpn.AccessAllow {
		t.Fatal("new owner not activated after retirement")
	}
}
func TestColdStartUnknownBindingStaysClosedUntilRetirement(t *testing.T) {
	r, a, _, req := accessReconcileFixture(t)
	revoker := &recordingAccessRevoker{failNext: true}
	r.Conntrack = revoker
	r.RequireInitialRetirement = true
	r.InitialBindings = map[string]bool{}
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected retirement failure")
	}
	for _, rule := range a.active {
		if rule.Action == vpn.AccessAllow {
			t.Fatal("retained old flow opened for unknown identity")
		}
	}
	reconcileAccess(t, r, req)
	if len(a.active) != 1 || a.active[0].Action != vpn.AccessAllow {
		t.Fatal("cold binding not activated")
	}
}

func TestColdStartVerifiedBindingPreservesItsPermittedFlows(t *testing.T) {
	r, a, c, req := accessReconcileFixture(t)
	leg := &lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: "l1-vpn", Namespace: "group"}, Spec: lab.LabVPNSpec{LabName: "l1", NetworkIndex: 1}}
	if err := c.Create(context.Background(), leg); err != nil {
		t.Fatal(err)
	}
	binding := vpn.AccessRule{ClientName: "p1", LabName: "l1", SourceCIDR: "10.8.0.2/32", DestinationCIDR: "10.8.1.0/24", LabInterface: "lab1", Action: vpn.AccessAllow}.BindingID()
	r.RequireInitialRetirement = true
	r.InitialBindings = map[string]bool{binding: true}
	revoker := &recordingAccessRevoker{failNext: true}
	r.Conntrack = revoker
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected conntrack failure")
	}
	if len(a.active) != 1 || a.active[0].Action != vpn.AccessAllow || revoker.lastRules[0].Action != vpn.AccessAllow {
		t.Fatal("verified same owner flow was unnecessarily closed")
	}
}

func TestStoppedFenceWaitsRetirementAndExactCurrentOperation(t *testing.T) {
	r, a, c, req := accessReconcileFixture(t)
	ctx := context.Background()
	_ = corev1.AddToScheme(c.Scheme())
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vpn-pod", Namespace: "group", UID: "vpn-uid", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "vpn", ContainerID: "containerd://1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	r.Reader = c
	r.GroupUID = "group-uid"
	r.Runtime = lab.VPNRuntimeIdentity{BootID: "boot1", PodName: pod.Name, PodUID: string(pod.UID), ContainerID: "containerd://1"}
	if err := r.initializeCurrentBoot(ctx, req.Namespace); err != nil {
		t.Fatal(err)
	}
	leg := &lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: names.LabVPNObjectName("l1"), Namespace: "group"}, Spec: lab.LabVPNSpec{LabName: "l1", NetworkIndex: 1}}
	leg.OwnerReferences = []metav1.OwnerReference{{Kind: "Lab", UID: "lab-uid", Name: "l1"}}
	if err := c.Create(ctx, leg); err != nil {
		t.Fatal(err)
	}
	revoker := &recordingAccessRevoker{}
	r.Conntrack = revoker
	reconcileAccess(t, r, req)
	var l lab.Lab
	_ = c.Get(ctx, client.ObjectKey{Name: "l1", Namespace: "group"}, &l)
	l.UID = "lab-uid"
	l.Generation = 2
	l.Spec.VPN.Enabled = true
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 2, SnapshotMode: "Required"}
	if err := c.Update(ctx, &l); err != nil {
		t.Fatal(err)
	}
	revoker.failNext = true
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("retirement failure hidden")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(leg), leg)
	if leg.Status.AccessFence != nil {
		t.Fatal("failure certified current access fence")
	}
	reconcileAccess(t, r, req)
	_ = c.Get(ctx, client.ObjectKeyFromObject(leg), leg)
	f := leg.Status.AccessFence
	if f == nil || f.OperationID != "stop" || f.Revision != 2 || f.LabUID != "lab-uid" || f.BootID != "boot1" || f.GroupUID != "group-uid" || f.ObservedGeneration != 2 {
		t.Fatal("missing exact physical fence", f)
	}
	for _, rule := range a.active {
		if rule.LabName == "l1" && rule.Action == vpn.AccessAllow {
			t.Fatal("stopped traffic allowed")
		}
	}
	calls := revoker.calls
	l.Spec.Lifecycle.Revision = 3
	l.Generation = 3
	_ = c.Update(ctx, &l)
	reconcileAccess(t, r, req)
	_ = c.Get(ctx, client.ObjectKeyFromObject(leg), leg)
	if leg.Status.AccessFence.Revision != 3 || revoker.calls <= calls {
		t.Fatal("same denied rules credited old retirement to new operation")
	}
}

func TestVPNBootPublicationInvalidatesOldFenceForSamePodAndReplacement(t *testing.T) {
	r, _, c, _ := accessReconcileFixture(t)
	ctx := context.Background()
	_ = corev1.AddToScheme(c.Scheme())
	r.Reader = c
	r.GroupUID = "group-uid"
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vpn-pod", Namespace: "group", UID: "pod-1", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "vpn", ContainerID: "containerd://1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []bool{false, true} {
		current, err := currentVPNRuntime(ctx, c, "group", p.Name, "old-boot")
		if err != nil {
			t.Fatal(err)
		}
		leg := &lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: names.LabVPNObjectName("l1"), Namespace: "group"}, Spec: lab.LabVPNSpec{LabName: "l1", NetworkIndex: 1}}
		if err := c.Get(ctx, client.ObjectKeyFromObject(leg), leg); err != nil {
			if err := c.Create(ctx, leg); err != nil {
				t.Fatal(err)
			}
		}
		leg.Status.Runtime = &current
		leg.Status.AccessFence = &lab.LabAccessFence{OperationID: "op", Revision: 1, VPNRuntimeIdentity: current}
		if err := c.Status().Update(ctx, leg); err != nil {
			t.Fatal(err)
		}
		if replacement {
			if err := c.Delete(ctx, p); err != nil {
				t.Fatal(err)
			}
			p.ResourceVersion = ""
			p.UID = "replacement"
			if err := c.Create(ctx, p); err != nil {
				t.Fatal(err)
			}
		} else {
			p.Status.ContainerStatuses[0].ContainerID = "containerd://2"
			p.Status.ContainerStatuses[0].RestartCount++
			if err := c.Status().Update(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		r.Runtime = current
		if err := r.publishBoot(ctx, "group"); err == nil {
			t.Fatal("old process published current boot after container/Pod replacement")
		}
		fresh, err := currentVPNRuntime(ctx, c, "group", p.Name, "new-boot")
		if err != nil {
			t.Fatal(err)
		}
		r.Runtime = fresh
		if err := r.initializeCurrentBoot(ctx, "group"); err != nil {
			t.Fatal(err)
		}
		if err := r.publishBoot(ctx, "group"); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(leg), leg); err != nil {
			t.Fatal(err)
		}
		if leg.Status.AccessFence != nil || leg.Status.Runtime == nil || leg.Status.Runtime.BootID != "new-boot" || leg.Status.Runtime.PodUID != string(p.UID) {
			t.Fatal("startup retained previous physical certificate", leg.Status)
		}
	}
}
func TestVPNNetworkStatusCannotOverwriteNewBootFenceFromStaleObject(t *testing.T) {
	r, _, c, _ := accessReconcileFixture(t)
	ctx := context.Background()
	leg := &lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: "leg", Namespace: "group"}, Spec: lab.LabVPNSpec{LabName: "l1", NetworkIndex: 1}}
	if err := c.Create(ctx, leg); err != nil {
		t.Fatal(err)
	}
	stale := leg.DeepCopy()
	leg.Status.Runtime = &lab.VPNRuntimeIdentity{BootID: "new"}
	leg.Status.AccessFence = &lab.LabAccessFence{OperationID: "op", Revision: 1, VPNRuntimeIdentity: lab.VPNRuntimeIdentity{BootID: "new"}}
	if err := c.Status().Update(ctx, leg); err != nil {
		t.Fatal(err)
	}
	network := &LabVPNReconciler{Client: r.Client}
	if err := network.patchStatus(ctx, stale, lab.LabVPNStatus{Phase: lab.LabVPNPhaseReady, DHCPReady: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(leg), leg); err != nil {
		t.Fatal(err)
	}
	if leg.Status.Runtime == nil || leg.Status.Runtime.BootID != "new" || leg.Status.AccessFence == nil || leg.Status.AccessFence.OperationID != "op" || !leg.Status.DHCPReady {
		t.Fatal("network status replacement lost foreign fields", leg.Status)
	}
}

func TestVPNStartupRebindsDelayedSamePodContainerStatusOnlyForCurrentProcess(t *testing.T) {
	r, _, c, req := accessReconcileFixture(t)
	ctx := context.Background()
	_ = corev1.AddToScheme(c.Scheme())
	r.Reader = c
	r.GroupUID = "group-uid"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vpn-pod", Namespace: "group", UID: "same-pod", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "vpn", ContainerID: "containerd://previous", RestartCount: 1, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	initial, err := currentVPNRuntime(ctx, c, "group", pod.Name, "new-process")
	if err != nil {
		t.Fatal(err)
	}
	r.Runtime = initial
	if err := r.initializeCurrentBoot(ctx, req.Namespace); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://current"
	pod.Status.ContainerStatuses[0].RestartCount = 2
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := r.publishBoot(ctx, req.Namespace); err != nil {
		t.Fatal("fresh process stayed permanently bound to old kubelet tuple", err)
	}
	if r.Runtime.ContainerID != "containerd://current" || r.Runtime.RestartCount != 2 {
		t.Fatal("delayed status did not bind current incarnation", r.Runtime)
	}
	var report lab.LabTrafficReport
	if err := c.Get(ctx, client.ObjectKey{Name: "vpn", Namespace: "group"}, &report); err != nil {
		t.Fatal(err)
	}
	if report.Status.CurrentVPNRuntime == nil || report.Status.CurrentVPNRuntime.ContainerID != r.Runtime.ContainerID {
		t.Fatal("independent witness not rebound")
	}
	report.Status.CurrentVPNRuntime.BootID = "newer-process"
	if err := c.Status().Update(ctx, &report); err != nil {
		t.Fatal(err)
	}
	if err := r.publishBoot(ctx, req.Namespace); err == nil {
		t.Fatal("superseded process restored its old boot over current startup report")
	}
}

func TestVPNStartupWaitsForDelayedInitialContainerStatus(t *testing.T) {
	_, _, c, _ := accessReconcileFixture(t)
	_ = corev1.AddToScheme(c.Scheme())
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vpn-pod", Namespace: "group", UID: "pod", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		time.Sleep(60 * time.Millisecond)
		p.Status.Phase = corev1.PodRunning
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "vpn", ContainerID: "containerd://new", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		ready <- c.Status().Update(context.Background(), p)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	identity, err := awaitVPNRuntime(ctx, c, "group", "vpn-pod", "boot")
	if err != nil || identity.ContainerID != "containerd://new" {
		t.Fatal("startup exited instead of waiting for kubelet identity", identity, err)
	}
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
}
