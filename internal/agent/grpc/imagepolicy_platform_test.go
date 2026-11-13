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

// A platform reference is the platform's own image: it passes with no policy of any kind, and the Lab runs the node form.
func TestCreateLabsMapsAPlatformImageToTheNodeAddress(t *testing.T) {
	h, _ := contractHandler(t, Features{})
	h.SetImagePolicy([]string{"ghcr.io/acme/private"}, "localhost:5035")
	if got := createdImages(t, h, platformImage); len(got) != 1 || got[0] != nodePlatform {
		t.Fatalf("the Lab runs %v, want %s", got, nodePlatform)
	}
}

// The node address is the registry as the nodes see it, set once for the agent.
func TestPlatformImagesUseTheConfiguredNodePrefix(t *testing.T) {
	h, _ := contractHandler(t, Features{})
	h.SetPlatformImages("localhost:6000", false)
	if got := createdImages(t, h, platformImage); len(got) != 1 || got[0] != "localhost:6000/platform/tools@"+platformDigest {
		t.Fatalf("the Lab runs %v", got)
	}
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

func platformOnlyHandler(t *testing.T, tenantFlag, platformWide bool) *Handler {
	t.Helper()
	ten := newTenantTenant("acme", false, nil)
	ten.Spec.Images.PlatformOnly = tenantFlag
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{ten})
	h.SetImagePolicy([]string{"ghcr.io/acme/private"}, "localhost:5035")
	h.SetPlatformImages("", platformWide)
	return h
}

// PlatformOnly, set by the tenant or for the whole platform, lets exactly platform/** by digest through.
func TestPlatformOnlyRefusesEverythingButPlatformImagesByDigest(t *testing.T) {
	for name, h := range map[string]*Handler{
		"tenant": platformOnlyHandler(t, true, false),
		"agent":  platformOnlyHandler(t, false, true),
	} {
		for _, bad := range []string{
			"nginx",
			"ghcr.io/acme/app:1",
			"ghcr.io/acme/app@" + platformDigest,
			"localhost:5035/ghcr.io/acme/app:1",                  // the cache
			"localhost:5035/lab/ns/lab/dev@" + platformDigest,    // a snapshot
			"localhost:5035/base@" + platformDigest,              // the shared base image
			"localhost:5035/platform/tools:1",                    // platform, but a tag
			"localhost:5035/platform/a/b@" + platformDigest,      // platform, but not one name
			"localhost:5000/platform/tools@" + platformDigest,    // another registry
			"other.example.com/platform/tools@" + platformDigest, // another host
			"localhost:5035/platformx/tools@" + platformDigest,   // another namespace
		} {
			err := createLabsWithImage(t, h, "acme", bad)
			if status.Code(err) != codes.PermissionDenied || !client.IsImageNotPlatform(err) {
				t.Errorf("%s, %q: want PermissionDenied with IMAGE_NOT_PLATFORM, got %v", name, bad, err)
			}
			if err != nil && !strings.Contains(err.Error(), `device "web"`) {
				t.Errorf("%s, %q: the message must name the device: %v", name, bad, err)
			}
		}
		for _, good := range []string{platformImage, nodePlatform} {
			if err := createLabsWithImage(t, h, "acme", good); status.Code(err) == codes.PermissionDenied {
				t.Errorf("%s, %q: %v", name, good, err)
			}
		}
	}
}

// With PlatformOnly set for the whole agent, the node form is mapped to itself and the Lab runs it.
func TestPlatformOnlyAgentRunsTheNodeForm(t *testing.T) {
	h, _ := contractHandler(t, Features{})
	h.SetPlatformImages("", true)
	if got := createdImages(t, h, nodePlatform); len(got) != 1 || got[0] != nodePlatform {
		t.Fatalf("the Lab runs %v", got)
	}
}

// Without PlatformOnly the node form of a platform image stays refused, like every reference that names the platform registry: a tenant
// names the logical form, which the platform checks and maps.
func TestWithoutPlatformOnlyTheNodeFormStaysRefused(t *testing.T) {
	h := platformOnlyHandler(t, false, false)
	err := createLabsWithImage(t, h, "acme", nodePlatform)
	if status.Code(err) != codes.PermissionDenied || client.IsImageNotPlatform(err) {
		t.Fatalf("the node form without platform-only: want the cache refusal, got %v", err)
	}
	// and an ordinary image still runs
	if err := createLabsWithImage(t, h, "acme", "nginx"); status.Code(err) == codes.PermissionDenied {
		t.Fatalf("an ordinary image: %v", err)
	}
}
