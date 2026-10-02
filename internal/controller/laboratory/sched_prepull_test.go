package laboratory

import (
	"fmt"
	"testing"


	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func pullRequest(nodes ...string) *laboratoryv1alpha1.ImagePull {
	return &laboratoryv1alpha1.ImagePull{Spec: laboratoryv1alpha1.ImagePullSpec{Nodes: nodes}}
}

func TestImagePullProgress(t *testing.T) {
	ip := pullRequest()
	if st := imagePullProgress(ip); !st.done || st.desired != 0 {
		t.Fatal("no eligible node: done")
	}
	ip = pullRequest("n1", "n2")
	if st := imagePullProgress(ip); st.done {
		t.Fatal("nothing reported yet: not done")
	}
	ip.Status.Nodes = map[string]laboratoryv1alpha1.NodeImagePull{
		"n1": {Done: true, Pulled: []string{"a"}},
		"n2": {Done: false, Pulled: []string{"a"}},
	}
	st := imagePullProgress(ip)
	if st.pulled != 1 || st.resolved != 1 || st.desired != 2 || st.done {
		t.Fatalf("progress = %+v", st)
	}
	ip.Status.Nodes["n2"] = laboratoryv1alpha1.NodeImagePull{Done: true}
	if st := imagePullProgress(ip); !st.done || len(st.failed) != 0 || st.pulled != 2 {
		t.Fatalf("all pulled: %+v", st)
	}
	// A node that is not asked does not count.
	ip.Status.Nodes["stranger"] = laboratoryv1alpha1.NodeImagePull{Done: true}
	if st := imagePullProgress(ip); st.resolved != 2 {
		t.Fatalf("only asked nodes count: %+v", st)
	}
}

// A pull error resolves the image at once: it is named once, and the prepull is done without it.
func TestImagePullProgressGivesUpOnAnImageThatCannotBePulled(t *testing.T) {
	ip := pullRequest("n1", "n2")
	bad := laboratoryv1alpha1.NodeImagePull{Done: true, Failed: []laboratoryv1alpha1.ImagePullFailure{{Image: "reg/broken:nope", Message: "manifest unknown"}}}
	ip.Status.Nodes = map[string]laboratoryv1alpha1.NodeImagePull{"n1": bad, "n2": bad}
	st := imagePullProgress(ip)
	if !st.done || st.pulled != 0 || len(st.failed) != 1 || st.failed[0] != "reg/broken:nope" {
		t.Fatalf("%+v", st)
	}
	// A slow pull (the node-agent has not finished) is still waited for.
	ip.Status.Nodes["n2"] = laboratoryv1alpha1.NodeImagePull{Done: false}
	if st := imagePullProgress(ip); st.done {
		t.Fatalf("a slow pull is not a failure: %+v", st)
	}
}

func TestBuildImagePull(t *testing.T) {
	var images []string
	for i := 0; i < maxPrepullImages+10; i++ {
		images = append(images, fmt.Sprintf("img-%d", i))
	}
	ip := buildImagePull(prepClass("acme", "g/x"), "acme", images, []string{"n1"})
	if len(ip.Spec.Images) != maxPrepullImages {
		t.Fatalf("images = %d", len(ip.Spec.Images))
	}
	if ip.Labels[managedByLabel] != managedByValue || ip.Spec.Tenant != "acme" || ip.Spec.PullSecret != "" {
		t.Fatalf("%+v", ip.Spec)
	}
	// The same group name of two tenants is two requests.
	if prepullName(prepClass("a", "g/x")) == prepullName(prepClass("b", "g/x")) {
		t.Fatal("the class key includes the tenant")
	}
}
