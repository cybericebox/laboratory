//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/ovn-org/libovsdb/ovsdb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func residualDeploymentAncestry(t *testing.T) (client.Client, *corev1.Pod, *lab.Device, *lab.Lab) {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = lab.AddToScheme(s)
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 3}}
	d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "device", Namespace: "ns", UID: "device", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", Name: "l", UID: l.UID}}}, Spec: lab.DeviceSpec{LabRef: "l"}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deployment", Namespace: "ns", UID: "deployment", OwnerReferences: []metav1.OwnerReference{{Kind: "Device", Name: d.Name, UID: d.UID}}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", UID: "rs", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: dep.Name, UID: dep.UID}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: "pod", OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID}}}}
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&corev1.Pod{}, "spec.nodeName", func(object client.Object) []string { return []string{object.(*corev1.Pod).Spec.NodeName} }).WithObjects(l, d, dep, rs, pod).Build()
	return c, pod, d, l
}
func TestResidualDeploymentAndDirectPodShareExactNativeOwner(t *testing.T) {
	c, p, d, l := residualDeploymentAncestry(t)
	ctx := context.Background()
	got, parent, err := nativePodDeviceLab(ctx, c, p)
	if err != nil || got.UID != d.UID || parent.UID != l.UID {
		t.Fatalf("Deployment native ancestry rejected: %v", err)
	}
	r := &LifecycleReporter{Reader: c}
	if owned, err := r.nativeDevicePodOwned(ctx, d, p); err != nil || !owned {
		t.Fatal("observation diverged from preparation ownership", err)
	}
	var rs appsv1.ReplicaSet
	_ = c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "rs"}, &rs)
	rs.UID = "replacement"
	_ = c.Update(ctx, &rs)
	if _, _, err := nativePodDeviceLab(ctx, c, p); !errors.Is(err, ErrPortOwnerChanged) {
		t.Fatalf("replacement ancestry accepted: %v", err)
	}
}
func TestResidualExternalRetirementWaitsForWholeWiringBoundary(t *testing.T) {
	ovs := &OVSManager{}
	r := &NetworkAttachReconciler{OVS: ovs, Flows: &FlowManager{}}
	// Model the interval after OVS registration while the peer is still moving
	// in the netns. An invalid key lets us stop before unrelated native adapters.
	for _, journaled := range []bool{false, true} {
		ovs.vethMu.Lock()
		done := make(chan error, 1)
		go func() {
			if journaled {
				done <- r.DelVethWithFlowsOwnedJournaled("invalid", "pod", func(string) error { return nil }, "row")
			} else {
				done <- r.DelVethWithFlowsExpected("invalid", "pod", "row")
			}
		}()
		select {
		case err := <-done:
			ovs.vethMu.Unlock()
			t.Fatalf("retirement entered during peer wiring: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		ovs.vethMu.Unlock()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("invalid-key fixture reached physical mutation")
			}
		case <-time.After(time.Second):
			t.Fatal("wiring unlock did not release retirement")
		}
	}
}
func TestResidualExternalRetirementRejectsPodRowAndOfportReplacement(t *testing.T) {
	for _, journaled := range []bool{false, true} {
		for _, scenario := range []string{"row-before", "pod-before", "row-at-barrier", "pod-at-barrier", "ofport-reused"} {
			t.Run(fmt.Sprintf("journaled-%v/%s", journaled, scenario), func(t *testing.T) {
				key := "p12345678"
				ids, _ := ovsdb.NewOvsMap(map[string]string{portKeyExternalID: key, portOwnerExternalID: "pod"})
				db := &residualFabricDB{ports: map[string]ovsdb.Row{key: {"_uuid": ovsdb.UUID{GoUUID: "original"}, "name": key, "external_ids": ids}}, interfaces: map[string]ovsdb.Row{}}
				if scenario == "row-before" {
					db.ports[key]["_uuid"] = ovsdb.UUID{GoUUID: "replacement"}
				}
				if scenario == "pod-before" {
					ids, _ = ovsdb.NewOvsMap(map[string]string{portKeyExternalID: key, portOwnerExternalID: "replacement"})
					db.ports[key]["external_ids"] = ids
				}
				flows := &FlowManager{client: scopeFlowAdapterWith(t, func() map[string]uint32 {
					if scenario == "ofport-reused" {
						return map[string]uint32{"replacement": 7}
					}
					return map[string]uint32{key: 7}
				}, func() {
					db.mu.Lock()
					defer db.mu.Unlock()
					if scenario == "row-at-barrier" {
						db.ports[key]["_uuid"] = ovsdb.UUID{GoUUID: "replacement"}
					}
					if scenario == "pod-at-barrier" {
						ids, _ := ovsdb.NewOvsMap(map[string]string{portKeyExternalID: key, portOwnerExternalID: "replacement"})
						db.ports[key]["external_ids"] = ids
					}
				})}
				r := &NetworkAttachReconciler{OVS: &OVSManager{client: db, ctx: context.Background(), bridge: "br-ovs"}, Flows: flows}
				persisted := false
				var err error
				if journaled {
					err = r.DelVethWithFlowsOwnedJournaled(key, "pod", func(string) error { persisted = true; return nil }, "original")
				} else {
					err = r.DelVethWithFlowsExpected(key, "pod", "original")
				}
				if err == nil || db.deletes != 0 || persisted {
					t.Fatalf("replacement retired through external entrypoint: %v deletes=%d receipt=%v", err, db.deletes, persisted)
				}
			})
		}
	}
}
