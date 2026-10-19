package laboratory

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cybericebox/laboratory/internal/names"
)

// The VPN and gateway pods of a new group are not safe to evict for a cluster autoscaler; a group that runs gets the
// annotation only with its next template change, never through a rollout of its own.
func TestGroupPodsSafeToEvictFalse(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	r := &LabGroupReconciler{Client: f.c, VPNBaseNetwork: "10.8.0.0/10", InetBaseNetwork: "10.9.0.0/10", VPNImage: "img:1", GatewayImage: "img:1"}
	ctx := context.Background()
	if err := r.ensureVPNDeployment(ctx, "lg-a", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureGatewayDeployment(ctx, "lg-a", false, nil); err != nil {
		t.Fatal(err)
	}
	get := func(name string) *appsv1.Deployment {
		t.Helper()
		var dep appsv1.Deployment
		if err := f.c.Get(ctx, types.NamespacedName{Namespace: "lg-a", Name: name}, &dep); err != nil {
			t.Fatal(err)
		}
		return &dep
	}
	for _, n := range []string{names.ComponentVPN, names.ComponentGateway} {
		if v := get(n).Spec.Template.Annotations[names.AnnotationSafeToEvict]; v != "false" {
			t.Fatalf("%s: safe-to-evict = %q", n, v)
		}
	}
	// a group from before: nothing else changes, so its pods are not restarted for the annotation
	old := get(names.ComponentVPN)
	delete(old.Spec.Template.Annotations, names.AnnotationSafeToEvict)
	if err := f.c.Update(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureVPNDeployment(ctx, "lg-a", false, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := get(names.ComponentVPN).Spec.Template.Annotations[names.AnnotationSafeToEvict]; ok {
		t.Fatal("a running group is not rolled out for the annotation alone")
	}
	// its next image update brings it
	r.VPNImage = "img:2"
	if err := r.ensureVPNDeployment(ctx, "lg-a", false, nil); err != nil {
		t.Fatal(err)
	}
	if v := get(names.ComponentVPN).Spec.Template.Annotations[names.AnnotationSafeToEvict]; v != "false" {
		t.Fatalf("after a template change safe-to-evict = %q", v)
	}
}
