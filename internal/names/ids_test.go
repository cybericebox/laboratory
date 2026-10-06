package names

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEncodeNameKeepsValidIDs(t *testing.T) {
	uuid := "0198c0a4-7a41-7000-8000-000000000001"
	if EncodeName(uuid) != uuid || EncodeName("e-1-t-1") != "e-1-t-1" {
		t.Fatal("a valid id is the name")
	}
	for _, id := range []string{"Upper", "has space", "a_b", strings.Repeat("a", 64), "-lead"} {
		n := EncodeName(id)
		if n == id || n[0] != 'h' || len(n) > 63 || n != EncodeName(id) {
			t.Fatalf("%q -> %q", id, n)
		}
	}
	if EncodeName("Upper") == EncodeName("upper") {
		t.Fatal("different ids, different names")
	}
}

func TestDeployKey(t *testing.T) {
	if DeployKey("") != "" || DeployKey("0198c0a4-7a41") != "0198c0a4-7a41" {
		t.Fatal("valid keys stay")
	}
	h := DeployKey("has space")
	if h[0] != 'h' || len(h) > 63 || h == DeployKey("other space") {
		t.Fatal(h)
	}
}

func TestValidateIDAndIDOf(t *testing.T) {
	for _, bad := range []string{"", strings.Repeat("a", 65), "a\nb"} {
		if ValidateID(bad) == nil {
			t.Errorf("%q", bad)
		}
	}
	if ValidateID("Any thing 1") != nil {
		t.Error("arbitrary strings are fine")
	}
	o := &metav1.ObjectMeta{Name: "x", Annotations: map[string]string{AnnotationID: "Orig"}}
	if IDOf(o) != "Orig" {
		t.Fatal(IDOf(o))
	}
	if IDOf(&metav1.ObjectMeta{Name: "x"}) != "x" {
		t.Fatal("falls back to the name")
	}
}
