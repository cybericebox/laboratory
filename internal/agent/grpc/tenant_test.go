package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// asClient is a call context carrying a verified client certificate with the given CN.
func asClient(cn string) context.Context {
	// issued now (the enrollment epoch refuses a certificate older than its tenant)
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-certBackdate)}
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}},
	})
}

func TestTenantOfTheCall(t *testing.T) {
	if got := tenantOf(context.Background()); got != names.DefaultTenant {
		t.Fatalf("no certificate: %q", got)
	}
	if got := tenantOf(asClient("backend-1")); got != "backend-1" {
		t.Fatalf("cn: %q", got)
	}
	long := strings.Repeat("x", 70)
	if got := tenantOf(asClient(long)); got == long || len(got) > 63 {
		t.Fatalf("a long cn is hashed: %q", got)
	}
	if got := tenantOf(asClient("")); got != names.DefaultTenant {
		t.Fatalf("empty cn: %q", got)
	}
}

func TestTenantsAreIsolated(t *testing.T) {
	h, _ := newFinalizerHandler(t)
	a, b := asClient("tenant-a"), asClient("tenant-b")

	res, err := h.CreateLabGroups(a, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "g-a", Labels: map[string]string{"team": "x"}}}})
	wantStates(t, res, err, stCreated)
	g, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(a, "g-a", metav1.GetOptions{})
	if g.Labels[names.LabelTenant] != "tenant-a" {
		t.Fatalf("the group is stamped: %v", g.Labels)
	}
	g.Status.Namespace = "ns-a"
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().UpdateStatus(a, g, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	spec := specJSON("web")
	res, err = h.CreateLabs(a, &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: spec}},
		Items:    []*protobuf.LabItem{{LabGroup: "g-a", Name: "c1", VariantId: "v"}},
	})
	wantStates(t, res, err, stCreated)
	if lab, _ := h.cs.LaboratoryV1alpha1().Labs("ns-a").Get(a, "c1", metav1.GetOptions{}); lab.Labels[names.LabelTenant] != "tenant-a" {
		t.Fatalf("the lab is stamped: %v", lab.Labels)
	}
	if acc, err := h.SetLabGroupAccess(a, &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{{LabGroupName: "g-a"}}}); err != nil || acc.Results[0].State != stCreated {
		t.Fatal(acc, err)
	}
	if p, _ := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies("ns-a").Get(a, names.LabGroupAccessPolicyName, metav1.GetOptions{}); p.Labels[names.LabelTenant] != "tenant-a" {
		t.Fatalf("the policy is stamped: %v", p.Labels)
	}
	// The stamp is never shown.
	if list, _ := h.ListLabGroups(a, &protobuf.ListRequest{}); len(list.Items) != 1 || len(list.Items[0].Labels) != 1 {
		t.Fatalf("tenant a sees its group without the stamp: %v", list)
	}

	// Tenant b sees and touches nothing of tenant a.
	if list, err := h.ListLabGroups(b, &protobuf.ListRequest{}); err != nil || len(list.Items) != 0 {
		t.Fatalf("list groups: %v %v", list, err)
	}
	if list, _ := h.ListLabGroups(b, &protobuf.ListRequest{Items: []*protobuf.ItemRef{{Name: "g-a"}}}); len(list.Items) != 0 {
		t.Fatalf("list by id: %v", list)
	}
	if list, _ := h.ListLabGroups(b, &protobuf.ListRequest{Selector: "team=x"}); len(list.Items) != 0 {
		t.Fatalf("a selector cannot reach into another tenant: %v", list)
	}
	if list, _ := h.ListLabs(b, &protobuf.ListRequest{}); len(list.Items) != 0 {
		t.Fatalf("list labs: %v", list)
	}
	if list, _ := h.ListLabs(b, &protobuf.ListRequest{LabGroup: "g-a"}); len(list.Items) != 0 {
		t.Fatalf("list labs of a foreign group: %v", list)
	}
	upd, err := h.UpdateLabGroups(b, &protobuf.UpdateLabGroupsRequest{
		Changes: &protobuf.LabGroupChanges{Suspended: protoBool(true)}, Items: []*protobuf.UpdateLabGroupItem{{Name: "g-a"}}})
	wantStates(t, upd, err, stNotFound)
	if upd, err = h.UpdateLabGroups(b, &protobuf.UpdateLabGroupsRequest{
		Changes: &protobuf.LabGroupChanges{Suspended: protoBool(true)}, BySelector: &protobuf.Selector{Selector: "team=x"}}); err != nil || len(upd.Results) != 0 {
		t.Fatalf("selector update: %v %v", upd, err)
	}
	del, err := h.DeleteLabGroups(b, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{Name: "g-a"}}})
	wantStates(t, del, err, stNotFound)
	if del, err = h.DeleteLabs(b, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{LabGroup: "g-a", Name: "c1"}}}); err != nil {
		t.Fatal(err)
	}
	wantStates(t, del, err, stNotFound)
	lr, err := h.CreateLabs(b, &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: spec}},
		Items:    []*protobuf.LabItem{{LabGroup: "g-a", Name: "c2", VariantId: "v"}},
	})
	wantStates(t, lr, err, stNotFound)
	acc, err := h.SetLabGroupAccess(b, &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{{LabGroupName: "g-a"}}})
	wantStates(t, acc, err, stNotFound)
	dev, err := h.ResetDevices(b, &protobuf.DevicesRequest{Items: []*protobuf.ItemRef{{LabGroup: "g-a", Lab: "c1", Name: "web"}}})
	wantStates(t, dev, err, stNotFound)
	if dev, err = h.ResetDevices(b, &protobuf.DevicesRequest{BySelector: &protobuf.DeviceSelector{Selector: "event", Device: "web"}}); err != nil || len(dev.Results) != 0 {
		t.Fatalf("device selector: %v %v", dev, err)
	}

	// Tenant b cannot take the id, and learns nothing about why.
	res, err = h.CreateLabGroups(b, &protobuf.CreateLabGroupsRequest{Items: groupItems("g-a")})
	wantStates(t, res, err, stFailed)
	if !strings.Contains(res.Results[0].Error, "not available") || strings.Contains(res.Results[0].Error, "exists") || res.Results[0].Retryable {
		t.Fatalf("the refusal must not reveal the object: %v", res.Results[0])
	}
	// Even a terminating object of another tenant looks the same.
	g, _ = h.cs.LaboratoryV1alpha1().LabGroups().Get(a, "g-a", metav1.GetOptions{})
	now := metav1.Now()
	g.DeletionTimestamp, g.Finalizers = &now, []string{"x"}
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().Update(a, g, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if res, err = h.CreateLabGroups(b, &protobuf.CreateLabGroupsRequest{Items: groupItems("g-a")}); err != nil || strings.Contains(res.Results[0].Error, "deleted") || res.Results[0].Retryable {
		t.Fatalf("terminating foreign object: %v %v", res, err)
	}

	// The reserved tenant label cannot be named in a selector or set.
	if _, err := h.ListLabGroups(b, &protobuf.ListRequest{Selector: names.LabelTenant + "=tenant-a"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("selector on the tenant label: %v", err)
	}
	if _, err := h.CreateLabGroups(b, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "g-b", Labels: map[string]string{names.LabelTenant: "tenant-a"}}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("setting the tenant label: %v", err)
	}
	// Tenant a still sees everything of its own.
	if list, _ := h.ListLabs(a, &protobuf.ListRequest{}); len(list.Items) != 1 {
		t.Fatalf("tenant a lost its lab: %v", list)
	}
}

func protoBool(b bool) *bool { return &b }

// Objects created before tenancy (no label) belong to the default tenant only.
func TestUnlabelledObjectsAreTheDefaultTenants(t *testing.T) {
	old := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}}
	h, _ := newFinalizerHandler(t, old)
	if list, _ := h.ListLabGroups(context.Background(), &protobuf.ListRequest{}); len(list.Items) != 1 {
		t.Fatalf("default tenant: %v", list)
	}
	if list, _ := h.ListLabGroups(asClient("someone"), &protobuf.ListRequest{}); len(list.Items) != 0 {
		t.Fatalf("another tenant: %v", list)
	}
}

func TestMonitoringIsFilteredByTenant(t *testing.T) {
	ga := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "ga", Labels: map[string]string{names.LabelTenant: "ta", "team": "x"}}, Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "nsa"}}
	gb := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "gb", Labels: map[string]string{names.LabelTenant: "tb", "team": "x"}}, Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "nsb"}}
	legacy := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "gl", Labels: map[string]string{"team": "x"}}, Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "nsl"}}
	// A lab without its own tenant label belongs to its group's tenant.
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "nsa"}}
	h, _ := newFinalizerHandler(t, ga, gb, legacy, lab)
	st, err := h.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	groups := func(tenant, sel string) (out []string, labs int) {
		f, err := newSelectorFilter(sel, tenant)
		if err != nil {
			t.Fatal(err)
		}
		u := f.snapshot(st)
		for _, g := range u.Groups {
			out = append(out, g.Name)
		}
		return out, len(u.Labs)
	}
	if g, l := groups("ta", ""); len(g) != 1 || g[0] != "ga" || l != 1 {
		t.Fatalf("tenant a: %v %d", g, l)
	}
	// The user's selector narrows within the tenant, it cannot widen past it.
	if g, _ := groups("tb", "team=x"); len(g) != 1 || g[0] != "gb" {
		t.Fatalf("tenant b: %v", g)
	}
	if g, _ := groups("ta", "team=y"); len(g) != 0 {
		t.Fatalf("selector narrows: %v", g)
	}
	if g, l := groups(names.DefaultTenant, ""); len(g) != 1 || g[0] != "gl" || l != 0 {
		t.Fatalf("default tenant: %v %d", g, l)
	}
	if st.update.Capacity != nil {
		t.Fatal("no cluster capacity in the shared observation: every subscriber gets its tenant's")
	}
}

// A sweep lists its own groups by label with their creation time, and never sees another tenant's.
func TestListLabGroupsBySelectorShowsCreationTimeOnlyOfOwnTenant(t *testing.T) {
	h, _ := newFinalizerHandler(t)
	a, b := asClient("tenant-a"), asClient("tenant-b")
	for _, c := range []struct {
		ctx  context.Context
		name string
	}{{a, "g-a"}, {b, "g-b"}} {
		res, err := h.CreateLabGroups(c.ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: c.name, Labels: map[string]string{"kind": "stand"}}}})
		wantStates(t, res, err, stCreated)
	}
	g, _ := h.cs.LaboratoryV1alpha1().LabGroups().Get(a, "g-a", metav1.GetOptions{})
	g.CreationTimestamp = metav1.NewTime(time.UnixMilli(1700000000123))
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().Update(a, g, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	list, err := h.ListLabGroups(a, &protobuf.ListRequest{Selector: "kind=stand"})
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != "g-a" || list.Items[0].CreatedUnixMs != 1700000000123 {
		t.Fatalf("tenant a: %v %v", list, err)
	}
	if list, err = h.ListLabGroups(b, &protobuf.ListRequest{Selector: "kind=stand"}); err != nil || len(list.Items) != 1 || list.Items[0].Name != "g-b" {
		t.Fatalf("tenant b sees only its own group: %v %v", list, err)
	}
}
