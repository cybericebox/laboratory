package grpc

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func TestLabGroupRecreateWhileTerminating(t *testing.T) {
	h, release := newFinalizerHandler(t)
	ctx := context.Background()

	if _, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "e-1-t-1"}); err != nil {
		t.Fatal(err)
	}
	// A live object: create stays the ordinary AlreadyExists (idempotent for callers).
	if _, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "e-1-t-1"}); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("live duplicate create: %v", err)
	}

	// The operator's finalizer keeps the group after Delete.
	lg, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, "e-1-t-1", metav1.GetOptions{})
	lg.Finalizers = []string{"laboratory/cleanup"}
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, lg, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DeleteLabGroup(ctx, &protobuf.IDRequest{Name: "e-1-t-1"}); err != nil {
		t.Fatal(err)
	}

	_, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "e-1-t-1"})
	requireTerminating(t, err)
	if got := status.Convert(err).Message(); got != "LabGroup e-1-t-1 is still being deleted, retry later" {
		t.Errorf("message: %q", got)
	}
	// Other writes are refused too, and Get still shows the dying object.
	_, err = h.SetLabGroupSuspended(ctx, &protobuf.LabGroupSuspendRequest{Name: "e-1-t-1", Suspended: true})
	requireTerminating(t, err)
	_, err = h.SetLabGroupVPNDisabled(ctx, &protobuf.LabGroupVPNDisabledRequest{Name: "e-1-t-1", Disabled: true})
	requireTerminating(t, err)
	_, err = h.UpdateLabGroup(ctx, &protobuf.LabGroup{Name: "e-1-t-1"})
	requireTerminating(t, err)
	_, err = h.ReconcileLabGroupAccess(ctx, &protobuf.LabGroupAccessPolicy{LabGroupName: "e-1-t-1"})
	requireTerminating(t, err)
	if _, err := h.GetLabGroup(ctx, &protobuf.IDRequest{Name: "e-1-t-1"}); err != nil {
		t.Fatalf("get while terminating: %v", err)
	}

	// Finalizer removed: the object is gone and create succeeds with a fresh one.
	release("labgroups", "", "e-1-t-1")
	if _, err := h.GetLabGroup(ctx, &protobuf.IDRequest{Name: "e-1-t-1"}); !apierrors.IsNotFound(err) {
		t.Fatalf("get after release: %v", err)
	}
	if _, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "e-1-t-1"}); err != nil {
		t.Fatalf("create after release: %v", err)
	}
}

func TestLabRecreateWhileTerminating(t *testing.T) {
	h, release := newFinalizerHandler(t)
	ctx := context.Background()
	specJSON, _ := json.Marshal(laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer},
	}})
	in := &protobuf.Lab{Namespace: "ns1", Name: "ch1", SpecJson: specJSON}

	if _, err := h.CreateLab(ctx, in); err != nil {
		t.Fatal(err)
	}
	lab, _ := h.cs.LaboratoryV1alpha1().Labs("ns1").Get(ctx, "ch1", metav1.GetOptions{})
	lab.Finalizers = []string{"laboratory/cleanup"}
	if _, err := h.cs.LaboratoryV1alpha1().Labs("ns1").Update(ctx, lab, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Live update stays an update.
	if _, err := h.UpdateLab(ctx, in); err != nil {
		t.Fatalf("live update: %v", err)
	}
	if _, err := h.DeleteLab(ctx, &protobuf.NamespacedIDRequest{Namespace: "ns1", Name: "ch1"}); err != nil {
		t.Fatal(err)
	}

	_, err := h.CreateLab(ctx, in)
	requireTerminating(t, err)
	_, err = h.UpdateLab(ctx, in)
	requireTerminating(t, err)

	release("labs", "ns1", "ch1")
	if _, err := h.CreateLab(ctx, in); err != nil {
		t.Fatalf("create after release: %v", err)
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

	_, err := h.CreateLabGroupClient(ctx, &protobuf.LabGroupClient{Namespace: "ns1", Name: "p-1"})
	requireTerminating(t, err)
	_, err = h.ReconcileLabGroupAccess(ctx, &protobuf.LabGroupAccessPolicy{LabGroupName: "g1"})
	requireTerminating(t, err)
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
