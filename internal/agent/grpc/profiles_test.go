package grpc

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func profileRequest(profile laboratoryv1alpha1.DeviceProfile) *protobuf.CreateLabsRequest {
	spec := laboratoryv1alpha1.LabSpec{}
	spec.VPN.Enabled = true
	spec.Devices = []laboratoryv1alpha1.DeviceTemplate{{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx", Profile: profile}}
	raw, _ := json.Marshal(spec)
	return &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: raw}},
		Items:    []*protobuf.LabItem{{Name: "a", LabGroup: "g", VariantId: "v"}},
	}
}

func addNode(t *testing.T, k kubernetes.Interface, name string, labels map[string]string) {
	t.Helper()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Status: corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}}
	created, err := k.CoreV1().Nodes().Create(context.Background(), n, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	created.Spec.Taints = nil // the API server taints a new node not-ready
	if created, err = k.CoreV1().Nodes().Update(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	created.Status = n.Status // the status is not written by a create
	if _, err := k.CoreV1().Nodes().UpdateStatus(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func wantProfileReason(t *testing.T, res *protobuf.BatchResult, reason string) {
	t.Helper()
	r := res.GetResults()[0]
	if r.State != protobuf.ItemState_ITEM_STATE_FAILED || r.Reason != reason || r.Retryable {
		t.Fatalf("want a final refusal %s, got %+v", reason, r)
	}
}

// A lab that asks for a profile that cannot run is refused with a reason and nothing is created: vm is reserved, microvm is off
// until the chart turns it on, and with it on it needs a running node that carries both labels (or a node group that can add one).
func TestCreateLabProfileRefusals(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)

	h.features.Profiles = nodecap.ProfilePolicy{}
	res, err := h.CreateLabs(ctx, profileRequest(laboratoryv1alpha1.DeviceProfileVM))
	wantStates(t, res, err, stFailed)
	wantProfileReason(t, res, client.ReasonProfileUnsupported)

	res, err = h.CreateLabs(ctx, profileRequest(laboratoryv1alpha1.DeviceProfileMicroVM))
	wantStates(t, res, err, stFailed)
	wantProfileReason(t, res, client.ReasonProfileUnsupported)

	res, err = h.CreateLabs(ctx, profileRequest("bogus"))
	wantStates(t, res, err, stFailed)
	wantProfileReason(t, res, client.ReasonProfileUnsupported)

	// switched on, but no node can run it
	h.features.Profiles = nodecap.ProfilePolicy{MicroVM: true}
	addNode(t, k, "plain", map[string]string{names.LabelProfileContainer: "true"})
	res, err = h.CreateLabs(ctx, profileRequest(laboratoryv1alpha1.DeviceProfileMicroVM))
	wantStates(t, res, err, stFailed)
	wantProfileReason(t, res, client.ReasonProfileUnavailable)
	if _, err := h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx, "a", metav1.GetOptions{}); err == nil {
		t.Fatal("a refused item must create nothing")
	}

	// a node group that can add such a node: accepted, the pods wait for it
	h.features.Profiles = nodecap.ProfilePolicy{MicroVM: true, ScaleUp: []string{"microvm"}}
	res, err = h.CreateLabs(ctx, profileRequest(laboratoryv1alpha1.DeviceProfileMicroVM))
	wantStates(t, res, err, stCreated)
}

func TestCreateLabMicroVMOnARunningNode(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	h.features.Profiles = nodecap.ProfilePolicy{MicroVM: true}
	addNode(t, k, "kvm", map[string]string{names.LabelProfileMicroVM: "true", names.LabelRuntimeKataCLH: "true"})
	res, err := h.CreateLabs(ctx, profileRequest(laboratoryv1alpha1.DeviceProfileMicroVM))
	wantStates(t, res, err, stCreated)
	lab, err := h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx, "a", metav1.GetOptions{})
	if err != nil || lab.Spec.Devices[0].Profile != laboratoryv1alpha1.DeviceProfileMicroVM {
		t.Fatalf("the profile must reach the Lab: %+v %v", lab, err)
	}
}

// A lab with no profile stays a container lab: nothing is written back and no node label is asked for.
func TestCreateLabWithoutProfileIsUnchanged(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	h.features.Profiles = nodecap.ProfilePolicy{MicroVM: true, Labels: true}
	res, err := h.CreateLabs(ctx, profileRequest(""))
	wantStates(t, res, err, stFailed) // profile labels are on and no node carries one
	wantProfileReason(t, res, client.ReasonProfileUnavailable)
	h.features.Profiles = nodecap.ProfilePolicy{}
	res, err = h.CreateLabs(ctx, profileRequest(""))
	wantStates(t, res, err, stCreated)
	lab, _ := h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx, "a", metav1.GetOptions{})
	if lab.Spec.Devices[0].Profile != "" {
		t.Fatalf("the agent must not write a default profile: %q", lab.Spec.Devices[0].Profile)
	}
}

// GetCapacity reports each offered profile: whether a running node can run it, whether scale-up is known and possible, the overhead.
func TestCapacityReportsNodeProfiles(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	h.features.Profiles = nodecap.ProfilePolicy{Labels: true, MicroVM: true, Overhead: nodecap.Amount{CPU: 50, Memory: 120 << 20}, ScaleUp: []string{"container", "microvm"}}
	addNode(t, k, "c", map[string]string{names.LabelProfileContainer: "true"})
	got, err := h.nodeProfiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []*protobuf.NodeProfileCapacity{
		{Profile: "container", AvailableNow: true, ScaleUpKnown: true, AvailableAfterScaleUp: true},
		{Profile: "microvm", AvailableNow: false, ScaleUpKnown: true, AvailableAfterScaleUp: true, OverheadCpuMillicores: 50, OverheadMemoryBytes: 120 << 20},
	}
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i].String() {
			t.Fatalf("profile %d: got %v want %v", i, got[i], want[i])
		}
	}

	// microvm off: absent from the list
	h.features.Profiles = nodecap.ProfilePolicy{}
	got, _ = h.nodeProfiles(ctx)
	if len(got) != 1 || got[0].Profile != "container" || !got[0].AvailableNow || got[0].ScaleUpKnown {
		t.Fatalf("only container, unknown scale-up: %v", got)
	}
}
