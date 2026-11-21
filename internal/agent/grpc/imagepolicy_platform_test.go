package grpc

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	platformDigest = "sha256:" + zeros64
	platformImage  = "platform:tools@" + platformDigest
	nodePlatform   = "localhost:5035/platform/tools@" + platformDigest
)

// createdImages creates a lab with one device of the given image in a ready group and returns the images of the Lab it made.
func createdImages(t *testing.T, h *Handler, image string) []string {
	t.Helper()
	res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: []byte(`{"devices":[{"name":"web","type":"container","image":"` + image + `"}]}`)}},
		Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "c1", VariantId: "v"}},
	})
	if err != nil {
		t.Fatalf("CreateLabs %q: %v", image, err)
	}
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetState() == protobuf.ItemState_ITEM_STATE_FAILED {
		t.Fatalf("CreateLabs %q: %v", image, res.GetResults())
	}
	lab, err := h.cs.LaboratoryV1alpha1().Labs("team-lab").Get(context.Background(), "c1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range lab.Spec.Devices {
		out = append(out, d.Image)
	}
	return out
}

// Other images are left exactly as they are.
func TestOrdinaryImagesAreNotMapped(t *testing.T) {
	h, _ := contractHandler(t, Features{})
	if got := createdImages(t, h, "ghcr.io/acme/app:1"); len(got) != 1 || got[0] != "ghcr.io/acme/app:1" {
		t.Fatalf("the Lab runs %v", got)
	}
}

// Anything that only looks like a platform reference is refused, and is never read as a Docker Hub image.
func TestMalformedPlatformReferencesAreRefused(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("acme", false, nil)})
	for _, bad := range []string{
		"platform:tools",                   // a name with no digest (would be library/platform:tools)
		"platform:tools:1",                 // a tag
		"platform:tools@sha256:abc",        // a short digest
		"platform:Tools@" + platformDigest, // upper case
		"platform:a/b@" + platformDigest,   // a path: the namespace is platform/<one name>
		"platform:../x@" + platformDigest,  // climbing
		"platform:tools@sha512:" + strings.Repeat("0", 128),
		"platform:" + strings.Repeat("a", 65) + "@" + platformDigest, // longer than the name limit
	} {
		err := createLabsWithImage(t, h, "acme", bad)
		if status.Code(err) != codes.PermissionDenied || !client.IsImageNotPlatform(err) {
			t.Errorf("%q: want PermissionDenied with IMAGE_NOT_PLATFORM, got %v", bad, err)
		}
	}
}

// The platform: scheme is gone: a reference of it is refused with IMAGE_NOT_PLATFORM, and the old node form is just a reference to the
// cache, which a tenant may not name.
func TestPlatformSchemeIsRefusedAndTheNodeFormStaysRefused(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("acme", false, nil)})
	h.SetImagePolicy([]string{"ghcr.io/acme/private"}, "localhost:5035")
	err := createLabsWithImage(t, h, "acme", platformImage)
	if status.Code(err) != codes.PermissionDenied || !client.IsImageNotPlatform(err) || !strings.Contains(err.Error(), `device "web"`) {
		t.Fatalf("%v", err)
	}
	err = createLabsWithImage(t, h, "acme", nodePlatform)
	if status.Code(err) != codes.PermissionDenied || client.IsImageNotPlatform(err) {
		t.Fatalf("the node form: want the cache refusal, got %v", err)
	}
	if err := createLabsWithImage(t, h, "acme", "nginx"); status.Code(err) == codes.PermissionDenied {
		t.Fatalf("an ordinary image: %v", err)
	}
}
