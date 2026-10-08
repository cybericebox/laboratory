//go:build linux

package nodeagent

import (
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"reflect"
	"testing"
)

func TestNativeRuntimeJournalFencesOperationAndNodeBoot(t *testing.T) {
	o := &NativeRuntimeObserver{JournalDir: t.TempDir()}
	id := lab.OwnedRuntimeIdentity{OwnerUID: "lab", PodUID: "pod", OperationID: "stop", Revision: 2, NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"container"}, CgroupPaths: []string{"/owned/cgroup"}, PortKeys: []string{"owned-port"}}
	receipt := cleanupReceipt{Identity: id, PortUUID: "actual-owner-row"}
	if err := o.writeRecord("cleanup-owned-port", id, receipt); err != nil {
		t.Fatal(err)
	}
	var recovered cleanupReceipt
	if err := o.readRecord("cleanup-owned-port", id, &recovered); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt, recovered) || recovered.Complete {
		t.Fatal("prepared transaction invented completed ACK")
	}
	for _, mutate := range []func(*lab.OwnedRuntimeIdentity){func(i *lab.OwnedRuntimeIdentity) { i.OperationID = "start" }, func(i *lab.OwnedRuntimeIdentity) { i.Revision++ }, func(i *lab.OwnedRuntimeIdentity) { i.NodeBootID = "next-boot" }, func(i *lab.OwnedRuntimeIdentity) { i.PodUID = "replacement" }} {
		stale := id
		mutate(&stale)
		if err := o.readRecord("cleanup-owned-port", stale, &recovered); err == nil {
			t.Fatal("stale identity consumed cleanup proof")
		}
	}
}
