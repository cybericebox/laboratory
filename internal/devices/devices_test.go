package devices

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestNameTellsPairsApart(t *testing.T) {
	if Name("a-b", "c") == Name("a", "b-c") {
		t.Fatal("lab a-b with device c and lab a with device b-c must not share a name")
	}
	if LegacyName("a-b", "c") != LegacyName("a", "b-c") {
		t.Fatal("the old scheme is the ambiguous one this replaces")
	}
	long := Name("0123456789012345678901234567890123456789", "abcdefghijabcdefghijabcdefghijabcde")
	if len(long) > 52 {
		t.Errorf("name too long: %d", len(long))
	}
	if Name("lab", "dev") != Name("lab", "dev") {
		t.Error("the name is deterministic")
	}
}

func dev(name, lab, device string) *laboratoryv1alpha1.Device {
	return &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       laboratoryv1alpha1.DeviceSpec{LabRef: lab, Name: device},
	}
}

func reader(t *testing.T, objs ...*laboratoryv1alpha1.Device) *fake.ClientBuilder {
	s := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b
}

func TestGetPrefersTheCurrentNameAndKeepsTheOldOne(t *testing.T) {
	ctx := context.Background()
	// a device made under the old name keeps being found
	c := reader(t, dev("a-b", "a", "b")).Build()
	got, err := Get(ctx, c, "ns", "a", "b")
	if err != nil || got.Name != "a-b" {
		t.Fatalf("legacy device: %v %v", got, err)
	}
	// a new one has the hashed name
	c = reader(t, dev(Name("a", "b"), "a", "b")).Build()
	if got, err = Get(ctx, c, "ns", "a", "b"); err != nil || got.Name != Name("a", "b") {
		t.Fatalf("new device: %v %v", got, err)
	}
}

// The collision of the old scheme: "a-b-c" belongs to lab a-b, and lab a with device b-c must neither find nor adopt it.
func TestGetDoesNotAdoptAnotherPairsDevice(t *testing.T) {
	c := reader(t, dev("a-b-c", "a-b", "c")).Build()
	if _, err := Get(context.Background(), c, "ns", "a", "b-c"); !apierrors.IsNotFound(err) {
		t.Fatalf("lab a must not find the device of lab a-b: %v", err)
	}
	if got, err := Get(context.Background(), c, "ns", "a-b", "c"); err != nil || got.Name != "a-b-c" {
		t.Fatalf("the owner finds it: %v %v", got, err)
	}
	if Name("a", "b-c") == "a-b-c" {
		t.Fatal("the new device of lab a must not take the old object's name")
	}
}
