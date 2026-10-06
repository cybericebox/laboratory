package grpc

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func createLabsWithImage(t *testing.T, h *Handler, tenant, image string) error {
	t.Helper()
	spec := `{"devices":[{"name":"web","type":"container","image":"` + image + `"}]}`
	_, err := h.CreateLabs(asClient(tenant), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: []byte(spec)}},
		Items:    []*protobuf.LabItem{{LabGroup: "g", Name: "l", VariantId: "v"}},
	})
	return err
}

func TestCreateLabsAppliesTheTenantImagePolicy(t *testing.T) {
	ten := newTenantTenant("acme", false, nil)
	ten.Spec.Images = laboratoryv1alpha1.TenantImages{Allow: []string{"ghcr.io/acme/", "docker.io/library"}}
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{ten})
	h.SetImagePolicy([]string{"ghcr.io/acme/private"}, "localhost:5035")

	for _, bad := range []string{
		"ghcr.io/platform/private:1",           // not on the allow list
		"ghcr.io/acme/private/tool:1",          // platform deny list wins over the tenant allow list
		"localhost:5035/ghcr.io/acme/app:1",    // names the cache directly
		"ghcr.io/acme/app:1@sha256:" + zeros64, // fine name, but check it passes below
	} {
		err := createLabsWithImage(t, h, "acme", bad)
		if bad == "ghcr.io/acme/app:1@sha256:"+zeros64 {
			if status.Code(err) == codes.PermissionDenied {
				t.Errorf("%s must pass the policy: %v", bad, err)
			}
			continue
		}
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: want PermissionDenied, got %v", bad, err)
		}
	}
	if err := createLabsWithImage(t, h, "acme", "nginx"); status.Code(err) == codes.PermissionDenied {
		t.Errorf("an allowed image: %v", err)
	}
}

const zeros64 = "0000000000000000000000000000000000000000000000000000000000000000"
