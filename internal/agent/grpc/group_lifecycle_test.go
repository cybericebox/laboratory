package grpc

import (
	"context"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestGroupLifecycleAcceptanceFencesAndNeverStartsChildren(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	g, err := h.getGroup(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	target := &protobuf.GroupTarget{Group: "g", ExpectedUid: string(g.UID), OperationId: "stop", Revision: 1}
	stop := &protobuf.StopLabGroupsRequest{Items: []*protobuf.StopLabGroupItem{{Target: target, RequireAllLabsStopped: true}}}
	res, err := h.StopLabGroups(ctx, stop)
	wantStates(t, res, err, stUpdated)
	res, err = h.StopLabGroups(ctx, stop)
	wantStates(t, res, err, stUpdated)
	after, _ := h.getGroup(ctx, "g")
	if p := labGroupToProto(after); p.GetStatus().GetLifecycle().GetObservedState() != "Unknown" {
		t.Fatal("acceptance claimed stop")
	}
	for _, mut := range []func(*protobuf.StopLabGroupsRequest){func(r *protobuf.StopLabGroupsRequest) { r.Items[0].RequireAllLabsStopped = false }, func(r *protobuf.StopLabGroupsRequest) { r.Items[0].Target.ExpectedUid = "foreign" }, func(r *protobuf.StopLabGroupsRequest) { r.Items[0].Target.OperationId = "conflict" }, func(r *protobuf.StopLabGroupsRequest) { r.Items[0].Target.Revision = 0 }} {
		request := proto.Clone(stop).(*protobuf.StopLabGroupsRequest)
		mut(request)
		res, err = h.StopLabGroups(ctx, request)
		wantStates(t, res, err, stFailed)
	}
	child := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "solved", Namespace: "g"}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "solved", Revision: 1, SnapshotMode: "Skip", Terminal: true}}}
	if _, err = h.cs.LaboratoryV1alpha1().Labs("g").Create(ctx, child, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	start := &protobuf.StartLabGroupsRequest{Items: []*protobuf.GroupTarget{{Group: "g", ExpectedUid: string(g.UID), OperationId: "start", Revision: 2}}}
	for n := 0; n < 2; n++ {
		res, err = h.StartLabGroups(ctx, start)
		wantStates(t, res, err, stUpdated)
	}
	unchanged, _ := h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx, "solved", metav1.GetOptions{})
	if !unchanged.Spec.Lifecycle.Terminal || !unchanged.Spec.Lifecycle.IsStopped() {
		t.Fatal("group start resurrected solved child")
	}
}
func TestGroupAdmissionTwoReplicasRestartBeforeAndAfterChildWrite(t *testing.T) {
	h, k := newTestHandler(t)
	second := NewHandler(h.cs, h.k8s, nil)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	readyGroup(t, h, k, "other", "other", nil)
	g, _ := h.getGroup(ctx, "g")
	other, _ := h.getGroup(ctx, "other")
	pending := &lab.GroupChildAdmission{GroupUID: string(g.UID), LabName: "new", DesiredState: "Running", SpecHash: "fingerprint"}
	if err := h.claimChildAdmission(ctx, "g", pending); err != nil {
		t.Fatal(err)
	}
	stop := &protobuf.StopLabGroupsRequest{Items: []*protobuf.StopLabGroupItem{{Target: &protobuf.GroupTarget{Group: "g", ExpectedUid: string(g.UID), OperationId: "stop", Revision: 1}, RequireAllLabsStopped: true}, {Target: &protobuf.GroupTarget{Group: "other", ExpectedUid: string(other.UID), OperationId: "stop", Revision: 1}, RequireAllLabsStopped: true}}}
	res, err := second.StopLabGroups(ctx, stop)
	wantStates(t, res, err, stFailed, stUpdated)
	if !res.Results[0].Retryable {
		t.Fatal("busy admission must be replayable")
	}
	// A restarted replica resumes the exact admitted operation without minting another token.
	if err := second.claimChildAdmission(ctx, "g", pending.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	otherPending := pending.DeepCopy()
	otherPending.LabName = "other-child"
	if err := second.claimChildAdmission(ctx, "g", otherPending); err == nil || !isRetryable(err) {
		t.Fatal("different operation cleared pending admission")
	}
	created := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "g", Annotations: map[string]string{names.AnnotationSpecHash: "fingerprint"}}}
	if _, err := h.cs.LaboratoryV1alpha1().Labs("g").Create(ctx, created, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Simulate crash after exact childwrite and before finish: GroupStop resolves
	// only that durable write and CASes stop on the same Group resourceVersion.
	res, err = second.StopLabGroups(ctx, &protobuf.StopLabGroupsRequest{Items: stop.Items[:1]})
	wantStates(t, res, err, stUpdated)
	g, _ = h.getGroup(ctx, "g")
	if g.Spec.Admission != nil || !g.Spec.Lifecycle.IsStopped() {
		t.Fatal("durable completion not resolved")
	}
	if err := h.claimChildAdmission(ctx, "g", pending); err == nil {
		t.Fatal("late worker admitted under stopped group")
	}
}

func TestGroupAdmissionConcurrentReplicaCASExcludesStopAndStart(t *testing.T) {
	h, k := newTestHandler(t)
	second := NewHandler(h.cs, h.k8s, nil)
	ctx := context.Background()
	readyGroup(t, h, k, "race", "race", nil)
	g, _ := h.getGroup(ctx, "race")
	gate := make(chan struct{})
	claimed := make(chan error, 1)
	stopped := make(chan *protobuf.BatchResult, 1)
	stopErr := make(chan error, 1)
	go func() {
		<-gate
		claimed <- h.claimChildAdmission(ctx, "race", &lab.GroupChildAdmission{GroupUID: string(g.UID), LabName: "pending", DesiredState: "Running", SpecHash: "spec"})
	}()
	go func() {
		<-gate
		res, err := second.StopLabGroups(ctx, &protobuf.StopLabGroupsRequest{Items: []*protobuf.StopLabGroupItem{{Target: &protobuf.GroupTarget{Group: "race", ExpectedUid: string(g.UID), OperationId: "stop", Revision: 1}, RequireAllLabsStopped: true}}})
		stopped <- res
		stopErr <- err
	}()
	close(gate)
	claimErr := <-claimed
	res := <-stopped
	if err := <-stopErr; err != nil {
		t.Fatal(err)
	}
	accepted := res.Results[0].State == stUpdated
	if claimErr == nil && accepted {
		t.Fatal("distinct replicas admitted start and stop together")
	}
	if claimErr != nil && !accepted {
		t.Fatalf("neither contender won: %v %v", claimErr, res)
	}
	live, _ := h.getGroup(ctx, "race")
	if accepted && live.Spec.Admission != nil || !accepted && live.Spec.Admission == nil {
		t.Fatal("CAS result lost durable pending state")
	}
}
