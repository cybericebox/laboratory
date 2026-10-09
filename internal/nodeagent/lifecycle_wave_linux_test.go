//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func waveNodeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	_ = lab.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()
}
func TestLifecycleWaveFirstCleanupRequiresDurableNativeInventory(t *testing.T) {
	o := &NativeRuntimeObserver{JournalDir: t.TempDir(), NodeName: "node", BootID: "boot"}
	if err := o.preparePortRetirement("owned-port", "pod", "row"); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("uninventoried cleanup permitted: %v", err)
	}
	// Native capture independently produces these identities. This adapter test
	// verifies durable obligation recovery, not containerd/cgroup absence.
	old := lab.OwnedRuntimeIdentity{OwnerUID: "lab", PodUID: "pod", NodeName: "node", NodeBootID: "boot", OperationID: "running1", Revision: 1, PortKeys: []string{"owned-port"}}
	stop := *old.DeepCopy()
	stop.OperationID = "stop2"
	stop.Revision = 2
	for _, id := range []lab.OwnedRuntimeIdentity{old, stop} {
		if err := o.recordObligation(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.preparePortRetirement("owned-port", "pod", "exact-row"); err != nil {
		t.Fatal(err)
	}
	restarted := &NativeRuntimeObserver{JournalDir: o.JournalDir, NodeName: "node", BootID: "boot"}
	for _, id := range []lab.OwnedRuntimeIdentity{old, stop} {
		var receipt cleanupReceipt
		if err := restarted.readRecord("cleanup-owned-port", id, &receipt); err != nil || receipt.PortUUID != "exact-row" || !reflect.DeepEqual(receipt.Identity, id) {
			t.Fatalf("current/historical obligation lost after first cleanup: %+v %v", receipt, err)
		}
	}
}
func TestLifecycleWaveConcurrentObligationCaptureRetainsEveryOperation(t *testing.T) {
	o := &NativeRuntimeObserver{JournalDir: t.TempDir()}
	var group sync.WaitGroup
	for revision := int64(1); revision <= 20; revision++ {
		group.Add(1)
		go func(rev int64) {
			defer group.Done()
			id := lab.OwnedRuntimeIdentity{OwnerUID: "lab", PodUID: "pod", NodeName: "node", NodeBootID: "boot", OperationID: "operation", Revision: rev}
			if err := o.recordObligation(id); err != nil {
				t.Error(err)
			}
		}(revision)
	}
	group.Wait()
	var rows []lab.OwnedRuntimeIdentity
	if err := o.readRecord("obligations", lab.OwnedRuntimeIdentity{PodUID: "pod", NodeName: "node", NodeBootID: "boot"}, &rows); err != nil || len(rows) != 20 {
		t.Fatalf("lost concurrent immutable obligations: %d %v", len(rows), err)
	}
}
func TestLifecycleWaveVNIACKCannotRetireReplacementOrNewOperation(t *testing.T) {
	binding := lab.OwnedVNI{Kind: "Connection", Namespace: "ns", Name: "connection", UID: "original", VNI: 7, OwnerUID: "lab", OperationID: "stop1", Revision: 1, Generation: 3}
	vni := uint(7)
	replacement := &lab.Connection{ObjectMeta: metav1.ObjectMeta{Name: "connection", Namespace: "ns", UID: "replacement"}, Status: lab.ConnectionStatus{VNI: &vni, VNILease: &lab.VNILease{PoolUID: "pool", Generation: 1}}}
	o := &NativeRuntimeObserver{JournalDir: t.TempDir(), NodeName: "node", BootID: "boot", Reader: waveNodeClient(t, replacement)}
	receipt := vniReceipt{Binding: binding, Complete: true}
	if err := o.writeRecord("vni-released", o.vniIdentity(binding), receipt); err != nil {
		t.Fatal(err)
	}
	// Recovery must consume the historical ACK without issuing a flow command
	// against the replacement's recycled numeric VNI; nil flows detects a bypass.
	if err := o.retireVNI(context.Background(), binding, nil); err != nil {
		t.Fatal(err)
	}
	newer := binding
	newer.OperationID = "stop3"
	newer.Revision = 3
	newer.Generation = 5
	if o.vniReleased(newer) {
		t.Fatal("old ACK certified a later lease barrier")
	}
	if err := o.retireVNI(context.Background(), newer, nil); err == nil {
		t.Fatalf("replacement numeric VNI deleted: %v", err)
	}
	o.BootID = "other-boot"
	if o.vniReleased(binding) {
		t.Fatal("unavailable/rebooted node certified old physical receipt")
	}
}
func TestLifecycleWaveConnectionKeepsFinalizerOnNativeReceiptFailure(t *testing.T) {
	ctx := context.Background()
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 3}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 1}}}
	vni := uint(7)
	conn := &lab.Connection{ObjectMeta: metav1.ObjectMeta{Name: "connection", Namespace: "ns", UID: "conn", Finalizers: []string{names.FinalizerOVSCleanup}}, Spec: lab.ConnectionSpec{LabRef: "l"}, Status: lab.ConnectionStatus{VNI: &vni, VNILease: &lab.VNILease{PoolUID: "pool", Generation: 1}}}
	c := waveNodeClient(t, l, conn)
	_ = c.Get(ctx, client.ObjectKeyFromObject(conn), conn)
	failed := errors.New("fsync receipt failed")
	seen := false
	r := &ConnectionReconciler{Client: c, Reader: c, NodeName: "node", OVS: &OVSManager{VNIRetirement: func(_ context.Context, b lab.OwnedVNI, _ *FlowManager) error {
		seen = true
		if b.UID != "conn" || b.OwnerUID != "lab" || b.OperationID != "stop" {
			t.Fatalf("unbound lease: %+v", b)
		}
		return failed
	}}, Flows: &FlowManager{}}
	if _, err := r.reconcileDelete(ctx, conn); !errors.Is(err, failed) {
		t.Fatalf("receipt error lost: %v", err)
	}
	var after lab.Connection
	_ = c.Get(ctx, client.ObjectKeyFromObject(conn), &after)
	if !seen || len(after.Finalizers) != 1 {
		t.Fatal("unacknowledged Connection cleanup dropped retry/finalizer")
	}
	stale := conn.DeepCopy()
	stale.UID = types.UID("previous")
	seen = false
	if _, err := r.reconcileDelete(ctx, stale); !errors.Is(err, ErrPortOwnerChanged) || seen {
		t.Fatalf("stale Connection authorized replacement cleanup: %v", err)
	}
}
func TestLifecycleWaveLegacyBootstrapIdentityDoesNotMutateIntent(t *testing.T) {
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 4}}
	op, rev := nativeLabOperation(l)
	if op == "" || rev != 1 || l.Spec.Lifecycle != nil {
		t.Fatal("bootstrap fabricated lifecycle intent")
	}
	o := &NativeRuntimeObserver{Reader: waveNodeClient(t, l), NodeName: "node", BootID: "boot"}
	scope := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", ScopeUID: "lab", OwnerUID: "lab", Namespace: "ns", LabName: "l", OperationID: op, Revision: rev, Generation: 4, NodeName: "node", NodeBootID: "boot"}
	if stopped, err := o.scopeCurrent(context.Background(), scope); err != nil || stopped {
		t.Fatalf("legacy running bootstrap rejected: %v %v", stopped, err)
	}
	scope.NodeBootID = "unavailable"
	if _, err := o.scopeCurrent(context.Background(), scope); err == nil {
		t.Fatal("wrong boot accepted native scope")
	}
}
