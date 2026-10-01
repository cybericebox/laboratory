package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// newFinalizerHandler returns a Handler over a fake clientset whose Delete keeps
// objects that carry finalizers (sets DeletionTimestamp) like a real API server;
// release(resource, ns, name) then simulates the finalizer being removed.
func newFinalizerHandler(t *testing.T, objs ...runtime.Object) (*Handler, func(resource, ns, name string)) {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	cs.PrependReactor("delete", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		del := action.(clienttesting.DeleteAction)
		obj, err := cs.Tracker().Get(del.GetResource(), del.GetNamespace(), del.GetName())
		if err != nil {
			return false, nil, nil
		}
		m := obj.(metav1.Object)
		if len(m.GetFinalizers()) == 0 {
			return false, nil, nil
		}
		now := metav1.Now()
		m.SetDeletionTimestamp(&now)
		if err := cs.Tracker().Update(del.GetResource(), obj, del.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})
	release := func(resource, ns, name string) {
		t.Helper()
		if err := cs.Tracker().Delete(laboratoryv1alpha1.SchemeGroupVersion.WithResource(resource), ns, name); err != nil {
			t.Fatalf("release %s/%s: %v", resource, name, err)
		}
	}
	return NewHandler(cs, k8sfake.NewSimpleClientset(), nil), release
}

func requireTerminating(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected terminating error, got nil")
	}
	if status.Code(err) != codes.Unavailable || !client.IsTerminating(err) {
		t.Fatalf("expected Unavailable/TERMINATING, got %v", err)
	}
}

// onlyResult unwraps the answer to a one-item call.
func onlyResult(t *testing.T, res *protobuf.BatchResult, err error) *protobuf.ItemResult {
	t.Helper()
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(res.GetResults()) != 1 {
		t.Fatalf("want one result, got %v", res.GetResults())
	}
	return res.GetResults()[0]
}

func requireItemTerminating(t *testing.T, r *protobuf.ItemResult) {
	t.Helper()
	if r.State != protobuf.ItemState_ITEM_STATE_FAILED || !r.Retryable || !strings.Contains(r.Error, "still being deleted") {
		t.Fatalf("expected a retryable terminating failure, got %v", r)
	}
}

func TestLabGroupRecreateWhileTerminating(t *testing.T) {
	h, release := newFinalizerHandler(t)
	ctx := context.Background()
	create := func() *protobuf.ItemResult {
		res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "e-1-t-1"}}})
		return onlyResult(t, res, err)
	}

	if r := create(); r.State != protobuf.ItemState_ITEM_STATE_CREATED {
		t.Fatal(r)
	}
	// A live object: the same create is EXISTS (idempotent for callers).
	if r := create(); r.State != protobuf.ItemState_ITEM_STATE_EXISTS {
		t.Fatalf("live duplicate create: %v", r)
	}

	// The operator's finalizer keeps the group after Delete.
	lg, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, "e-1-t-1", metav1.GetOptions{})
	lg.Finalizers = []string{"laboratory/cleanup"}
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, lg, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	del, err := h.DeleteLabGroups(ctx, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{Name: "e-1-t-1"}}})
	if r := onlyResult(t, del, err); r.State != protobuf.ItemState_ITEM_STATE_DELETED {
		t.Fatal(r)
	}

	r := create()
	requireItemTerminating(t, r)
	if r.Error != "LabGroup e-1-t-1 is still being deleted, retry later" {
		t.Errorf("message: %q", r.Error)
	}
	// Other writes are refused too, and the list still shows the dying object.
	upd, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{
		Changes: &protobuf.LabGroupChanges{Suspended: proto.Bool(true)},
		Items:   []*protobuf.UpdateLabGroupItem{{Name: "e-1-t-1"}},
	})
	requireItemTerminating(t, onlyResult(t, upd, err))
	acc, err := h.SetLabGroupAccess(ctx, &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{{LabGroupName: "e-1-t-1"}}})
	requireItemTerminating(t, onlyResult(t, acc, err))
	list, err := h.ListLabGroups(ctx, &protobuf.ListRequest{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list while terminating: %v %v", list, err)
	}

	// Finalizer removed: the object is gone and create succeeds with a fresh one.
	release("labgroups", "", "e-1-t-1")
	if list, _ := h.ListLabGroups(ctx, &protobuf.ListRequest{}); len(list.Items) != 0 {
		t.Fatalf("list after release: %v", list)
	}
	if r := create(); r.State != protobuf.ItemState_ITEM_STATE_CREATED {
		t.Fatalf("create after release: %v", r)
	}
}

func TestLabRecreateWhileTerminating(t *testing.T) {
	h, release := newFinalizerHandler(t,
		&laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1"}, Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "ns1"}})
	ctx := context.Background()
	specJSON, _ := json.Marshal(laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer},
	}})
	create := func() *protobuf.ItemResult {
		res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON}},
			Items:    []*protobuf.LabItem{{LabGroup: "g1", Name: "ch1", VariantId: "v"}},
		})
		return onlyResult(t, res, err)
	}

	if r := create(); r.State != protobuf.ItemState_ITEM_STATE_CREATED {
		t.Fatal(r)
	}
	lab, _ := h.cs.LaboratoryV1alpha1().Labs("ns1").Get(ctx, "ch1", metav1.GetOptions{})
	lab.Finalizers = []string{"laboratory/cleanup"}
	if _, err := h.cs.LaboratoryV1alpha1().Labs("ns1").Update(ctx, lab, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// A live resend stays EXISTS.
	if r := create(); r.State != protobuf.ItemState_ITEM_STATE_EXISTS {
		t.Fatalf("live resend: %v", r)
	}
	del, err := h.DeleteLabs(ctx, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{LabGroup: "g1", Name: "ch1"}}})
	if r := onlyResult(t, del, err); r.State != protobuf.ItemState_ITEM_STATE_DELETED {
		t.Fatal(r)
	}

	requireItemTerminating(t, create())
	upd, err := h.UpdateLabs(ctx, &protobuf.UpdateLabsRequest{
		Changes: &protobuf.LabChanges{Labels: &protobuf.LabelChanges{Set: map[string]string{"a": "b"}}},
		Items:   []*protobuf.UpdateLabItem{{LabGroup: "g1", Name: "ch1"}},
	})
	requireItemTerminating(t, onlyResult(t, upd, err))

	release("labs", "ns1", "ch1")
	if r := create(); r.State != protobuf.ItemState_ITEM_STATE_CREATED {
		t.Fatalf("create after release: %v", r)
	}
}

func TestLabGroupClientAndPolicyWhileTerminating(t *testing.T) {
	now := metav1.Now()
	fin := []string{"laboratory/cleanup"}
	h, _ := newFinalizerHandler(t,
		&laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p-1", Namespace: "ns1", DeletionTimestamp: &now, Finalizers: fin}},
		&laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1"}, Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "ns1"}},
		&laboratoryv1alpha1.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "ns1", DeletionTimestamp: &now, Finalizers: fin}},
	)
	ctx := context.Background()

	res, err := h.CreateLabGroupClients(ctx, &protobuf.CreateLabGroupClientsRequest{Items: []*protobuf.LabGroupClientItem{{LabGroup: "g1", Name: "p-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	requireItemTerminating(t, res.Results[0].Result)
	acc, err := h.SetLabGroupAccess(ctx, &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{{LabGroupName: "g1"}}})
	requireItemTerminating(t, onlyResult(t, acc, err))
}

func TestAPIErrorToStatus(t *testing.T) {
	gr := laboratoryv1alpha1.SchemeGroupVersion.WithResource("labgroups").GroupResource()
	cases := map[codes.Code]error{
		codes.NotFound:      apierrors.NewNotFound(gr, "x"),
		codes.AlreadyExists: apierrors.NewAlreadyExists(gr, "x"),
		codes.Aborted:       apierrors.NewConflict(gr, "x", nil),
	}
	for want, in := range cases {
		if got := status.Code(apiErrorToStatus(in)); got != want {
			t.Errorf("%v: got %v want %v", in, got, want)
		}
	}
	if err := terminatingError("Lab", "x"); apiErrorToStatus(err) != err {
		t.Error("gRPC statuses must pass through")
	}
}
