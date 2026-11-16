package grpc

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// An image from a registry that is not on the laboratory's list is refused with the stable reason, whatever the tenant's own list says;
// the same image passes where the list is empty or holds "*".
func TestCreateLabsRefusesARegistryThatIsNotAllowed(t *testing.T) {
	ten := newTenantTenant("acme", false, nil)
	ten.Spec.Images = laboratoryv1alpha1.TenantImages{Allow: []string{"gcr.io/"}} // a tenant cannot widen the laboratory's list
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{ten})
	h.SetAllowedRegistries([]string{"docker.io", "ghcr.io"})
	err := createLabsWithImage(t, h, "acme", "gcr.io/acme/web:1")
	if status.Code(err) != codes.PermissionDenied || !client.IsRegistryNotAllowed(err) {
		t.Fatalf("want PermissionDenied REGISTRY_NOT_ALLOWED, got %v", err)
	}
	if client.IsImageNotPlatform(err) {
		t.Errorf("a registry refusal is not a platform refusal: %v", err)
	}
	for _, listed := range []string{"nginx", "ghcr.io/acme/app:1"} {
		if err := createLabsWithImage(t, h, "acme", listed); client.IsRegistryNotAllowed(err) {
			t.Errorf("%s is on the list: %v", listed, err)
		}
	}
	for _, list := range [][]string{nil, {"*"}, {"docker.io", "*"}} {
		h.SetAllowedRegistries(list)
		if err := createLabsWithImage(t, h, "acme", "gcr.io/acme/web:1"); client.IsRegistryNotAllowed(err) {
			t.Errorf("list %v allows every registry: %v", list, err)
		}
	}
}

// GetFeatures tells the list, in canonical form, with the platform's own registries the chart added.
func TestFeaturesReportAllowedRegistries(t *testing.T) {
	for _, c := range []struct {
		list []string
		any  bool
		want []string
	}{
		{nil, true, nil},
		{[]string{"*"}, true, nil},
		{[]string{"docker.io", "GHCR.io", "Registry.Example.com:5000", "registry.example.com:443"}, false, []string{"docker.io", "ghcr.io", "registry.example.com:5000", "registry.example.com"}},
	} {
		h := featuresHandler(t, testFeatures, newTenantTenant("a", true, nil))
		h.SetAllowedRegistries(c.list)
		got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		r := got.GetAllowedRegistries()
		if r == nil || r.GetAny() != c.any || len(r.GetRegistries()) != len(c.want) {
			t.Fatalf("%v: %+v", c.list, r)
		}
		for i := range c.want {
			if r.GetRegistries()[i] != c.want[i] {
				t.Errorf("%v: %+v", c.list, r)
			}
		}
	}
}
