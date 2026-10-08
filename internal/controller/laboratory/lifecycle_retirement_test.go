package laboratory

import (
	"context"
	"encoding/json"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestRetirementRequiresNewNativeChallengeNotCrossNodeClock(t *testing.T) {
	id := lab.OwnedRuntimeIdentity{OwnerUID: "lab", OperationID: "stop", Revision: 2, PodUID: "pod", NodeName: "node", NodeBootID: "kernel", ContainerIDs: []string{"id"}, CgroupPaths: []string{"group"}, PortKeys: []string{"owned"}}
	when := metav1.NewTime(time.Unix(200, 0))
	in := lab.LifecycleRetirementIntent{ExpectedUID: "lab", StopOperationID: "stop", StopRevision: 2, OperationID: "retire", Revision: 3, RequestedAt: metav1.NewTime(time.Unix(100, 0))}
	report := lab.OwnedRuntimeReport{Identity: id, RuntimeState: "Released", ObservedAt: &when, RuntimeAbsentAt: &when, CgroupAbsentAt: &when, AttachmentsAbsentAt: &when}
	if freshRetirementRows([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{report}, in) {
		t.Fatal("old release accepted from timestamp alone")
	}
	report.RetirementOperationID = "retire"
	report.RetirementRevision = 3
	// An independent node clock need not be newer than the agent request clock.
	earlier := metav1.NewTime(time.Unix(50, 0))
	report.ObservedAt = &earlier
	if !freshRetirementRows([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{report}, in) {
		t.Fatal("exact new native challenge rejected by clock skew")
	}
	report.Identity.PodUID = "replacement"
	if freshRetirementRows([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{report}, in) {
		t.Fatal("replacement native owner accepted")
	}
}
func TestRetirementTombstoneRequiresExactReceipt(t *testing.T) {
	in := lab.LifecycleRetirementIntent{ExpectedUID: "lab", StopOperationID: "stop", StopRevision: 2, Generation: 7, OperationID: "retire", Revision: 3, RequestedAt: metav1.NewTime(time.Unix(100, 0))}
	raw, _ := json.Marshal(in)
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "lab", Generation: 8, Annotations: map[string]string{names.AnnotationLifecycleRetirement: string(raw)}}}
	status := retirementObservation(in, 8)
	status.ObservedAt = &metav1.Time{Time: time.Unix(101, 0)}
	status.RuntimeAbsent = true
	status.StorageState = "Deleted"
	status.State = "Deleted"
	status.CleanupComplete = true
	l.Status.Retirement = status
	if !LifecycleRetired(l) {
		t.Fatal("exact scrubbed-generation receipt rejected")
	}
	for _, mutate := range []func(*lab.Lab){func(x *lab.Lab) { x.Status.Retirement.OperationID = "foreign" }, func(x *lab.Lab) { x.Status.Retirement.StopRevision++ }, func(x *lab.Lab) { x.Status.Retirement.ObservedGeneration-- }, func(x *lab.Lab) { x.Status.Retirement.CleanupComplete = false }, func(x *lab.Lab) { x.Status.Retirement.Error = "failed" }} {
		copy := l.DeepCopy()
		mutate(copy)
		if LifecycleRetired(copy) {
			t.Fatal("invalid retirement acknowledged")
		}
	}
}
func TestRetirementCleanupRejectsReplacementSecretOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	// Actual cleanup cases include Core Secret/Device/Connection ownership and
	// finalizer/retry paths in the END native gate. This unit guards typed identity.
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "original", Namespace: "ns", Name: "l"}}
	replacement := &lab.Device{ObjectMeta: metav1.ObjectMeta{UID: "replacement", Namespace: "ns", Name: "device", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", Name: "l", UID: "other"}}}, Spec: lab.DeviceSpec{LabRef: "l"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(l, replacement).Build()
	var got lab.Device
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(replacement), &got); err != nil {
		t.Fatal(err)
	}
	if ownedLabDevice(l, &got) {
		t.Fatal("replacement owner accepted")
	}
}

func TestRetirementCleanupPreservesSameNameReplacement(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	replacement := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "env", UID: "replacement", OwnerReferences: []metav1.OwnerReference{{UID: "lab"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(replacement).Build()
	sweep := &RetentionSweeper{Client: c, Reader: c}
	receipt := []lab.RetirementObjectIdentity{{Kind: "Secret", Name: "env", UID: "original", OwnerUID: "lab"}}
	if err := sweep.cleanupRetirementObjects(context.Background(), c, "ns", receipt, func() error { return nil }); err == nil {
		t.Fatal("replacement cleanup accepted")
	}
	var kept corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(replacement), &kept); err != nil {
		t.Fatal("replacement deleted", err)
	}
}
