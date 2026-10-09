package grpc

import (
	"context"
	"reflect"
	"strings"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	typed "github.com/cybericebox/laboratory/clientset/client/versioned/typed/laboratory/v1alpha1"
	devnames "github.com/cybericebox/laboratory/internal/devices"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRejectedExistingLabCreateDoesNotBlockSiblingAdmission(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "preflight", "preflight", nil)
	request := &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("box")}},
		Items:    []*protobuf.LabItem{{LabGroup: "preflight", Name: "existing", VariantId: "v"}},
	}
	got, err := h.CreateLabs(ctx, request)
	wantStates(t, got, err, stCreated)
	labs := h.cs.LaboratoryV1alpha1().Labs("preflight")
	original, err := labs.Get(ctx, "existing", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Run the mismatched item first, independently of batch goroutine scheduling.
	request.Variants[0].SpecJson = specJSON("box", "extra")
	got, err = h.CreateLabs(ctx, request)
	wantStates(t, got, err, stFailed)
	if !strings.Contains(got.Results[0].Error, "different spec") {
		t.Fatalf("expected immutable spec rejection: %v", got.Results[0])
	}
	request.Items[0].Name = "sibling"
	request.Variants[0].SpecJson = specJSON("box")
	got, err = h.CreateLabs(ctx, request)
	wantStates(t, got, err, stCreated)
	current, err := labs.Get(ctx, "existing", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != original.ResourceVersion || !reflect.DeepEqual(current.Spec, original.Spec) {
		t.Fatal("rejected create mutated the existing lab")
	}
	group, err := h.getGroup(ctx, "preflight")
	if err != nil {
		t.Fatal(err)
	}
	if group.Spec.Admission != nil {
		t.Fatal("completed sibling left a pending admission")
	}

	// A previously claimed unknown outcome keeps its barrier, even for a known conflict.
	pending := &lab.GroupChildAdmission{GroupUID: string(group.UID), LabName: "unfinished", DesiredState: "Running", SpecHash: "unknown-outcome"}
	if err := h.claimChildAdmission(ctx, "preflight", pending); err != nil {
		t.Fatal(err)
	}
	request.Items[0].Name = "existing"
	request.Variants[0].SpecJson = specJSON("box", "extra")
	got, err = h.CreateLabs(ctx, request)
	wantStates(t, got, err, stFailed)
	if !got.Results[0].Retryable {
		t.Fatal("preexisting admission barrier was bypassed")
	}
	group, err = h.getGroup(ctx, "preflight")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(group.Spec.Admission, pending) {
		t.Fatal("rejected create cleared or replaced an unknown pending admission")
	}
}

// Delay one real Create until a concurrent API write changes the preflighted Lab.
// The actual API still enforces resource versions and AlreadyExists behavior.
type admissionRaceClient struct {
	versioned.Interface
	beforeCreate func() error
}

func (c admissionRaceClient) LaboratoryV1alpha1() typed.LaboratoryV1alpha1Interface {
	return admissionRaceAPI{c.Interface.LaboratoryV1alpha1(), c.beforeCreate}
}

type admissionRaceAPI struct {
	typed.LaboratoryV1alpha1Interface
	beforeCreate func() error
}

func (c admissionRaceAPI) Labs(namespace string) typed.LabInterface {
	return admissionRaceLabs{c.LaboratoryV1alpha1Interface.Labs(namespace), c.beforeCreate}
}

type admissionRaceLabs struct {
	typed.LabInterface
	beforeCreate func() error
}

func (c admissionRaceLabs) Create(ctx context.Context, object *lab.Lab, options metav1.CreateOptions) (*lab.Lab, error) {
	if err := c.beforeCreate(); err != nil {
		return nil, err
	}
	return c.LabInterface.Create(ctx, object, options)
}

func TestLabCreateRevalidatesExistingSpecAfterPreflight(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "preflight-race", "preflight-race", nil)
	request := &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("box")}},
		Items:    []*protobuf.LabItem{{LabGroup: "preflight-race", Name: "existing", VariantId: "v", Env: []*protobuf.DeviceEnv{envOf("box", "FLAG", "original")}}},
	}
	got, err := h.CreateLabs(ctx, request)
	wantStates(t, got, err, stCreated)
	original := h.cs
	h.cs = admissionRaceClient{original, func() error {
		labs := original.LaboratoryV1alpha1().Labs("preflight-race")
		current, err := labs.Get(ctx, "existing", metav1.GetOptions{})
		if err != nil {
			return err
		}
		current.Annotations[names.AnnotationSpecHash] = "concurrently-changed-spec"
		_, err = labs.Update(ctx, current, metav1.UpdateOptions{})
		return err
	}}
	request.Items[0].Env = []*protobuf.DeviceEnv{envOf("box", "FLAG", "replacement")}
	got, err = h.CreateLabs(ctx, request)
	wantStates(t, got, err, stFailed)
	if !strings.Contains(got.Results[0].Error, "different spec") {
		t.Fatalf("stale preflight authorized a changed spec: %v", got.Results[0])
	}
	group, err := h.getGroup(ctx, "preflight-race")
	if err != nil {
		t.Fatal(err)
	}
	if group.Spec.Admission == nil || group.Spec.Admission.LabName != "existing" {
		t.Fatal("post-claim rejection discarded its durable admission barrier")
	}
	secret, err := k.CoreV1().Secrets("preflight-race").Get(ctx, devnames.Name("existing", "box")+"-env", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["FLAG"]) != "original" {
		t.Fatal("stale preflight rewrote device secrets")
	}
}
