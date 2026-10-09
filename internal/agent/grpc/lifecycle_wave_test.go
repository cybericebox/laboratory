package grpc

import (
	"context"
	"encoding/json"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	labclient "github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLifecycleWaveAcceptedStartMasksOldReadyAndAccess(t *testing.T) {
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "lab", Generation: 4}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Running", OperationID: "start2", Revision: 2}}, Status: lab.LabStatus{Phase: lab.PhaseReady, VPN: lab.LabNetworkStatus{Ready: true}, Internet: lab.LabNetworkStatus{Ready: true}, Devices: []lab.DeviceRef{{Name: "device", Ready: true}}, Access: []lab.AccessEntry{{URL: "old-access"}}, Lifecycle: &lab.LabLifecycleStatus{LabUID: "lab", OperationID: "stop1", Revision: 1, ObservedGeneration: 3, ObservedState: "Stopped"}}}
	for _, project := range []func(*lab.Lab) *protobuf.Lab{func(l *lab.Lab) *protobuf.Lab { return labToProto(l) }, func(l *lab.Lab) *protobuf.Lab { return labProjection(l, false) }} {
		p := project(l).GetStatus()
		if p.GetReady() || p.GetVpnReady() || p.GetInternetReady() || len(p.GetAccess()) != 0 || p.GetDevices()[0].GetReady() {
			t.Fatal("accepted Start exposed stale readiness/access", p)
		}
	}
	l.Status.Lifecycle = &lab.LabLifecycleStatus{LabUID: "lab", OperationID: "start2", Revision: 2, ObservedGeneration: 4, ObservedState: "Running"}
	if !labToProto(l).GetStatus().GetReady() {
		t.Fatal("exact current Running masked forever")
	}
}
func TestLifecycleWaveGroupStartCannotPublishOldReleasedZero(t *testing.T) {
	g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{UID: "group", Generation: 4}, Spec: lab.LabGroupSpec{Lifecycle: &lab.GroupLifecycleSpec{DesiredState: "Running", OperationID: "start2", Revision: 2}}, Status: lab.LabGroupStatus{Resources: &lab.RuntimeAllocation{RuntimeState: "Released", OperationID: "stop1", Revision: 1, ConfiguredRequests: lab.ResourceAmounts{CPUMillicores: 75, MemoryBytes: 112 << 20}}}}
	p := labGroupToProto(g).GetStatus().GetResources()
	if p.GetRuntimeState() == "Released" || p.GetOperationId() != "start2" || p.GetLifecycleRevision() != 2 || p.GetAllocatedRequests().GetCpuMillicores() != 75 || p.GetAllocatedRequests().GetMemoryBytes() != 112<<20 {
		t.Fatalf("old group release reused on Start: %+v", p)
	}
}
func TestLifecycleWaveBirthReceiptCommitsActualUIDAndPreventsRebind(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	group, err := h.getGroup(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	spec := lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Running", OperationID: "birth-running1", Revision: 1}}
	raw, _ := json.Marshal(spec)
	request := &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{{VariantId: "birth", SpecJson: raw}}, Items: []*protobuf.LabItem{{Name: "a", LabGroup: "g", VariantId: "birth", ExpectedGroupUid: string(group.UID)}}}
	res, err := h.CreateLabs(ctx, request)
	wantStates(t, res, err, stCreated)
	actual, err := h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	receipt := creationReceiptToProto(actual)
	hash, err := labclient.CreationDefinitionHash(raw, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || !receipt.GetCommitted() || receipt.GetLabUid() != string(actual.UID) || receipt.GetGroupUid() != string(group.UID) || receipt.GetDefinitionHash() != hash || receipt.GetCreationId() == "" || receipt.GetNamespaceUid() == "" {
		t.Fatalf("birth not bound to actual committed UID: %+v", receipt)
	}
	group, _ = h.getGroup(ctx, "g")
	if group.Spec.Admission != nil || len(group.Status.Creations) != 1 || !group.Status.Creations[0].Committed {
		t.Fatal("create admission released before actual UID receipt")
	}
	// Legacy deletion is allowed after the committed receipt, but it supplies no
	// retirement credit. Repeating the original birth can never bind a replacement.
	uid := actual.UID
	if err := h.cs.LaboratoryV1alpha1().Labs("g").Delete(ctx, "a", metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
		t.Fatal(err)
	}
	res, err = h.CreateLabs(ctx, request)
	wantStates(t, res, err, stFailed)
	if _, err = h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx, "a", metav1.GetOptions{}); err == nil {
		t.Fatal("unknown old birth recreated under the same operation")
	}
}
func TestLifecycleWavePendingBirthBlocksDeleteAndAdmissionRelease(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	g, _ := h.getGroup(ctx, "g")
	pending := &lab.GroupChildAdmission{GroupUID: string(g.UID), LabName: "a", DesiredState: "Running", SpecHash: "hash"}
	if err := h.claimChildAdmission(ctx, "g", pending); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(lab.LabCreationReceipt{CreationID: "pending", GroupUID: string(g.UID), OperationID: "original", Revision: 1})
	actual := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g", Annotations: map[string]string{names.AnnotationLabCreation: string(raw), names.AnnotationSpecHash: "hash"}}}
	if _, err := h.cs.LaboratoryV1alpha1().Labs("g").Create(ctx, actual, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if h.childAdmissionWritten(ctx, g, pending) {
		t.Fatal("spec hash alone released uncommitted birth admission")
	}
	res, err := h.DeleteLabs(ctx, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{LabGroup: "g", Name: "a"}}})
	wantStates(t, res, err, stFailed)
}
