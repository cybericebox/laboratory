//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
)

// A Connection finalizer that asks again for the exact row this node-agent already retired gets its cleanup ACK: the
// row UUID is unique forever, so "absent and retired by us" is proof. Without that, the second pass of a finalizer (or
// the finalizer after the Pod path took the row) found no row and the Lab never went away.
func TestNetnsRetiredExactRowIsAcknowledged(t *testing.T) {
	nstest.Require(t)
	ovs, flows, _, sock := nativeOwnerOVS(t)
	key := names.DevicePortKey("g", "dev", "eth0")
	const uid = types.UID("pod-uid")
	if err := ovs.AddVethPortOwned(key, uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ovs.DelVethPort(key) })
	row := strings.TrimSpace(nstest.Run(t, "", "ovs-vsctl", "--db=unix:"+sock, "get", "Port", key, "_uuid"))
	if err := flows.AddT0Port(key, 7); err != nil {
		t.Fatal(err)
	}
	r := &NetworkAttachReconciler{OVS: ovs, Flows: flows}

	if err := r.DelVethWithFlowsOwned(key, uid); err != nil {
		t.Fatal(err)
	}
	if err := r.DelVethWithFlowsExpected(key, uid, row); err != nil {
		t.Fatalf("the exact row retired by this node-agent must be acknowledged: %v", err)
	}
	if err := r.DelVethWithFlowsExpected(key, uid, row); err != nil {
		t.Fatalf("a repeated finalizer pass must be acknowledged again: %v", err)
	}
	if err := r.DelVethWithFlowsExpected(key, uid, "00000000-0000-0000-0000-000000000001"); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("a row this node-agent never retired must not be acknowledged: %v", err)
	}
	if err := r.DelVethWithFlowsExpected(key, "other-uid", row); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("another Pod incarnation must not be acknowledged: %v", err)
	}
	if err := r.DelVethWithFlowsOwned(key, uid); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("without the exact row a missing owner row still cannot ACK: %v", err)
	}
}

// The Pod path has nothing to clean for a port that is in neither OVS nor the kernel (a Connection finalizer already
// retired it): it must not fail the Pod's reconcile forever. A kernel link without an OVS owner still fails.
func TestNetnsPodCleanupSkipsAPortThatIsGone(t *testing.T) {
	nstest.Require(t)
	ovs, flows, _, _ := nativeOwnerOVS(t)
	kube, _, _ := ownerPods(t, names.ComponentVPN)
	r := &NetworkAttachReconciler{Client: kube, Reader: kube, NodeName: "node", OVS: ovs, Flows: flows}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "g", UID: "pod-uid"}}
	gone := names.DevicePortKey("g", "dev", "eth0")
	if err := r.cleanupPodPort(context.Background(), pod, gone); err != nil {
		t.Fatalf("a port absent from OVS and the kernel needs no cleanup: %v", err)
	}
	stray := names.DevicePortKey("g", "dev", "eth1")
	if err := ovs.AddVethPortOwned(stray, "pod-uid"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ovs.DelVethPort(stray) })
	ops, _ := ovs.portSnapshotLocked(stray)
	if ops == nil {
		t.Fatal("fixture port has no row")
	}
	if err := ovs.delPortLocked(ops); err != nil { // the row goes, the kernel link stays
		t.Fatal(err)
	}
	if err := r.cleanupPodPort(context.Background(), pod, stray); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("a kernel link without an OVS owner must still fail: %v", err)
	}
}
