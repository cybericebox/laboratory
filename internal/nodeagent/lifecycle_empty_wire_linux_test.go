//go:build linux

package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestEmptyNativeScopeRequiredWireArrays(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Released" || report.Error != "" {
		t.Fatalf("native adapter did not release empty scope: %+v", report)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("CICE_SCOPE_WIRE_REPORT_FILE"); path != "" {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"containerIDs", "cgroupPaths", "portKeys"} {
		if _, ok := wire["identity"].(map[string]any)[key].([]any); !ok {
			t.Errorf("required identity.%s is not an array: %s", key, raw)
		}
	}
}

func TestEmptyNativeCachedReportRequiredWireArrays(t *testing.T) {
	o, id := scopeProducerAdapter(t, nil, nil)
	report := o.ObserveScope(context.Background(), id)
	if report.RuntimeState != "Released" {
		t.Fatal(report)
	}
	legacy := report
	legacy.Identity.ContainerIDs = nil
	legacy.Identity.CgroupPaths = nil
	legacy.Identity.PortKeys = nil
	if err := o.writeRecord("scope-fabric-released", report.Identity, legacy); err != nil {
		t.Fatal(err)
	}
	var durable lab.OwnedRuntimeReport
	if err := o.readRecord("scope-fabric-released", report.Identity, &durable); err != nil {
		t.Fatal("cached fixture record/key mismatch:", err)
	}
	if !committedRuntimeReport(durable, durable.Identity) {
		t.Fatal("cached fixture lacks exact successful release/timestamps")
	}
	raw, _ := json.Marshal(report)
	var reference lab.OwnedRuntimeReport
	_ = json.Unmarshal(raw, &reference)
	onlyShape := durable.Identity
	onlyShape.ContainerIDs, onlyShape.CgroupPaths, onlyShape.PortKeys = reference.Identity.ContainerIDs, reference.Identity.CgroupPaths, reference.Identity.PortKeys
	if !reflect.DeepEqual(onlyShape, reference.Identity) {
		t.Fatal("cached fixture differs beyond required empty-array shape")
	}
	if err := o.Runtime.Close(); err != nil {
		t.Fatal(err)
	}
	cached := o.ObserveScope(context.Background(), report.Identity)
	if cached.RuntimeState != "Released" || cached.Error != "" || cached.Identity.ContainerIDs == nil || cached.Identity.CgroupPaths == nil || cached.Identity.PortKeys == nil {
		t.Fatalf("cached empty receipt cannot publish legal arrays: %+v", cached)
	}
	wrong := report.Identity
	wrong.ScopeUID = "foreign"
	if got := o.ObserveScope(context.Background(), wrong); got.RuntimeState == "Released" {
		t.Fatal("wire shape normalization minted foreign release")
	}
}

func TestEmptyNativeReporterBadLabDoesNotStarveFollowingLab(t *testing.T) {
	o, _ := scopeProducerAdapter(t, nil, nil)
	scheme := o.Reader.(client.Client).Scheme()
	makeLab := func(name string) *lab.Lab {
		id := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", ScopeUID: name, OwnerUID: name, Namespace: "ns", LabName: name, Generation: 2, OperationID: "stop", Revision: 1, NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{}, CgroupPaths: []string{}, PortKeys: []string{}}
		return &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name), Generation: 2}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 1}}, Status: lab.LabStatus{ScopeInventory: []lab.OwnedRuntimeIdentity{id}, Resources: &lab.RuntimeAllocation{RuntimeState: "Unknown", StorageState: "Unknown", AllocatedRequests: lab.ResourceAmounts{CPUMillicores: 10}}}}
	}
	bad, good := makeLab("a-bad"), makeLab("b-good")
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&lab.Lab{}).WithObjects(bad, good).WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string { return []string{obj.(*corev1.Pod).Spec.NodeName} }).Build()
	o.Reader = c
	publicationErr := apierrors.NewInvalid(lab.SchemeGroupVersion.WithKind("Lab").GroupKind(), bad.Name, field.ErrorList{field.Invalid(field.NewPath("status", "scopeReports"), nil, "rejected first object")})
	writer := interceptor.NewClient(c, interceptor.Funcs{SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if obj.GetName() == bad.Name {
			return publicationErr
		}
		return cl.Status().Patch(ctx, obj, patch, opts...)
	}})
	r := &LifecycleReporter{Client: writer, Reader: c, Observer: o}
	if err := r.sync(context.Background()); !errors.Is(err, publicationErr) {
		t.Fatalf("publication rejection not surfaced: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(good), good); err != nil {
		t.Fatal(err)
	}
	if len(good.Status.ScopeReports) != 1 || good.Status.ScopeReports[0].RuntimeState != "Released" {
		t.Fatalf("first rejected object starved following native row: %+v", good.Status.ScopeReports)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(bad), bad); err != nil {
		t.Fatal(err)
	}
	if len(bad.Status.ScopeReports) != 0 || bad.Status.Resources.RuntimeState != "Unknown" || bad.Status.Resources.AllocatedRequests.CPUMillicores != 10 {
		t.Fatal("rejected row gained release credit")
	}
}
