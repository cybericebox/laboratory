package registrygate

import (
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/names"
)

func TestTenantSegmentsMapBackAndCollisionsMapToNobody(t *testing.T) {
	s := NewSchedule()
	s.SetTenant(TenantInfo{Name: "acme"})
	s.SetTenant(TenantInfo{Name: "Weird.Name"})
	for seg, want := range map[string]string{
		"acme":                            "acme",
		names.TenantSegment("Weird.Name"): "Weird.Name",
		names.DefaultTenant:               names.DefaultTenant, // an object without a tenant label needs no Tenant object
	} {
		if got, ok := s.TenantOfSegment(seg); !ok || got != want {
			t.Errorf("%q -> %q %v, want %q", seg, got, ok, want)
		}
	}
	for _, seg := range []string{"nobody", "platform", "t-000000000000", ""} {
		if _, ok := s.TenantOfSegment(seg); ok {
			t.Errorf("%q must map to no tenant", seg)
		}
	}
	// a tenant named "platform" is encoded, so it can never read the platform's credentials
	s.SetTenant(TenantInfo{Name: "platform"})
	if _, ok := s.TenantOfSegment("platform"); ok {
		t.Error("the segment platform is never a tenant")
	}
	if got, ok := s.TenantOfSegment(names.TenantSegment("platform")); !ok || got != "platform" {
		t.Errorf("a tenant called platform has an encoded segment: %q %v", got, ok)
	}
	s.DeleteTenant("acme")
	if _, ok := s.TenantOfSegment("acme"); ok {
		t.Error("a deleted tenant")
	}
}

func TestPodsAreIndexedByNodeAndMoveWithThem(t *testing.T) {
	s := NewSchedule()
	s.SetPod(PodInfo{Namespace: "a", Name: "p", Node: "n1"})
	s.SetPod(PodInfo{Namespace: "a", Name: "q", Node: "n1"})
	if got := s.PodsOnNode("n1"); len(got) != 2 || got[0].Name != "p" || got[1].Name != "q" {
		t.Fatalf("%v", got)
	}
	s.SetPod(PodInfo{Namespace: "a", Name: "p", Node: "n2"})
	if len(s.PodsOnNode("n1")) != 1 || len(s.PodsOnNode("n2")) != 1 {
		t.Fatal("a pod that changed node left the first one")
	}
	s.DeletePod("a", "p")
	s.DeletePod("a", "q")
	if len(s.PodsOnNode("n1"))+len(s.PodsOnNode("n2")) != 0 {
		t.Fatal("deleted pods remain")
	}
}

func TestFreshnessWindow(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := NewSchedule()
	s.Now = func() time.Time { return now }
	s.Confirm()
	now = now.Add(15 * time.Minute)
	if !s.Fresh(15 * time.Minute) {
		t.Fatal("exactly at the limit")
	}
	now = now.Add(time.Second)
	if s.Fresh(15 * time.Minute) {
		t.Fatal("past the limit")
	}
}

func TestImagePullsForNode(t *testing.T) {
	s := NewSchedule()
	s.SetImagePull(ImagePullInfo{Name: "a", Nodes: []string{"n1", "n2"}})
	s.SetImagePull(ImagePullInfo{Name: "b", Nodes: []string{"n2"}})
	if got := s.ImagePullsForNode("n1"); len(got) != 1 || got[0].Name != "a" {
		t.Fatal(got)
	}
	if got := s.ImagePullsForNode("n2"); len(got) != 2 {
		t.Fatal(got)
	}
	s.DeleteImagePull("a")
	if got := s.ImagePullsForNode("n1"); len(got) != 0 {
		t.Fatal(got)
	}
}
