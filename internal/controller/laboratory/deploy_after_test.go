package laboratory

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
)

// The annotation holds the original keys; the scheduler compares them with the group
// labels, which hold names.DeployKey of the originals.
func TestDeployAfterMapsThroughDeployKey(t *testing.T) {
	obj := &metav1.ObjectMeta{Annotations: map[string]string{names.AnnotationDeployAfter: "intro, Has Space ,"}}
	got := deployAfter(obj)
	if len(got) != 2 || got[0] != "intro" || got[1] != names.DeployKey("Has Space") || got[1] == "Has Space" {
		t.Fatalf("got %v", got)
	}
}
