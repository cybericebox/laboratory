package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// CreateLabGroupClients generates the keypair, registers the public key, waits for the
// reconciler to assemble the config, and substitutes the real private key for the
// placeholder. envtest has no reconciler, so a goroutine simulates it.
func TestLabGroupClientsEndToEnd(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k8s, "team-cli", "team-cli", nil)
	simulateClientReconciler(t, h, "team-cli")

	res, err := h.CreateLabGroupClients(ctx, &protobuf.CreateLabGroupClientsRequest{
		Labels: map[string]string{"event": "e1"},
		Items: []*protobuf.LabGroupClientItem{
			{LabGroup: "team-cli", Name: "alice", Labels: map[string]string{"role": "a"}},
			{LabGroup: "team-cli", Name: "bob"},
			{LabGroup: "nope", Name: "carol"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	states := []protobuf.ItemState{stCreated, stCreated, stNotFound}
	for i, want := range states {
		if res.Results[i].Result.State != want {
			t.Fatalf("result %d: %v", i, res.Results[i].Result)
		}
	}
	out := res.Results[0].Client
	cr, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-cli").Get(ctx, "alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cr: %v", err)
	}
	if cr.Spec.PublicKey == "" {
		t.Errorf("expected a generated public key on the CR")
	}
	if cr.Labels["event"] != "e1" || cr.Labels["role"] != "a" {
		t.Errorf("labels: %v", cr.Labels)
	}
	// The private key never touches the cluster: the CR config still holds the
	// placeholder, but the returned config has the real key substituted in.
	if !strings.Contains(cr.Status.Config, names.WGPrivateKeyPlaceholder) {
		t.Errorf("CR config should keep the placeholder: %q", cr.Status.Config)
	}
	if strings.Contains(out.Status.Config, names.WGPrivateKeyPlaceholder) || !strings.Contains(out.Status.Config, "PrivateKey = ") {
		t.Errorf("returned config: %q", out.Status.Config)
	}

	// A resend is EXISTS with the placeholder config (the key is gone); a new label UPDATED.
	again, err := h.CreateLabGroupClients(ctx, &protobuf.CreateLabGroupClientsRequest{Items: []*protobuf.LabGroupClientItem{
		{LabGroup: "team-cli", Name: "alice", Labels: map[string]string{"role": "a"}},
		{LabGroup: "team-cli", Name: "bob", Labels: map[string]string{"role": "b"}},
	}})
	if err != nil || again.Results[0].Result.State != stExists || again.Results[1].Result.State != stUpdated {
		t.Fatalf("resend: %v %v", again, err)
	}
	if !strings.Contains(again.Results[0].Client.Status.Config, names.WGPrivateKeyPlaceholder) {
		t.Fatal("an existing client never comes back with a private key")
	}

	// List by names, by selector.
	list, err := h.ListLabGroupClients(ctx, &protobuf.ListRequest{LabGroup: "team-cli", Selector: "role=b"})
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != "bob" || list.Items[0].LabGroupName != "team-cli" {
		t.Fatalf("list selector: %v %v", list, err)
	}
	list, _ = h.ListLabGroupClients(ctx, &protobuf.ListRequest{Items: []*protobuf.ItemRef{{LabGroup: "team-cli", Name: "alice"}, {LabGroup: "team-cli", Name: "zed"}}})
	if len(list.Items) != 1 || list.Items[0].Name != "alice" {
		t.Fatalf("list names: %v", list)
	}

	// Update labels by selector, then delete by selector with the guard.
	upd, err := h.UpdateLabGroupClients(ctx, &protobuf.UpdateLabGroupClientsRequest{
		BySelector: &protobuf.Selector{Selector: "event=e1", LabGroup: "team-cli", ExpectedCount: proto.Int64(2)},
		Labels:     &protobuf.LabelChanges{Set: map[string]string{"round": "2"}, Remove: []string{"role"}},
	})
	wantStates(t, upd, err, stUpdated, stUpdated)
	cr, _ = h.cs.LaboratoryV1alpha1().LabGroupClients("team-cli").Get(ctx, "alice", metav1.GetOptions{})
	if cr.Labels["round"] != "2" || cr.Labels["role"] != "" {
		t.Fatalf("labels after update: %v", cr.Labels)
	}
	if _, err = h.DeleteLabGroupClients(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "round=2", ExpectedCount: proto.Int64(1)}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("guard: %v", err)
	}
	del, err := h.DeleteLabGroupClients(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "round=2", ExpectedCount: proto.Int64(2)}})
	wantStates(t, del, err, stDeleted, stDeleted)
	del, err = h.DeleteLabGroupClients(ctx, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{LabGroup: "team-cli", Name: "alice"}}})
	wantStates(t, del, err, stNotFound)
}

func TestListLabGroupClientsCarriesStatistics(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k8s, "team-stats", "team-stats", nil)
	lgc := &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "carol", Namespace: "team-stats"}}
	created, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-stats").Create(ctx, lgc, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	created.Status.Statistics.RxBytes = 100
	created.Status.Statistics.TxBytes = 200
	created.Status.Statistics.LastHandshake = metav1.NewTime(time.Unix(1_700_000_000, 0))
	if _, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-stats").UpdateStatus(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	list, err := h.ListLabGroupClients(ctx, &protobuf.ListRequest{LabGroup: "team-stats"})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	st := list.Items[0].Status.Statistics
	if st.RxBytes != 100 || st.TxBytes != 200 || st.LastHandshakeUnix != 1_700_000_000 {
		t.Fatalf("statistics: %+v", st)
	}
}

func TestSetLabGroupAccessEndToEnd(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	const namespace = "team-access"
	readyGroup(t, h, k8s, "event-team", namespace, map[string]string{"event": "e1"})
	for _, name := range []string{"web", "forensics"} {
		if _, err := h.cs.LaboratoryV1alpha1().Labs(namespace).Create(ctx, &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create lab %q: %v", name, err)
		}
	}
	allow, deny := protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW, protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_DENY
	policy := func(rules ...*protobuf.LabGroupAccessRule) *protobuf.SetLabGroupAccessRequest {
		return &protobuf.SetLabGroupAccessRequest{
			Labels:   map[string]string{"scope": "access"},
			Policies: []*protobuf.LabGroupAccessPolicy{{LabGroupName: "event-team", Rules: rules}},
		}
	}
	rules := []*protobuf.LabGroupAccessRule{
		{Action: allow, ClientNames: []string{"alice"}, LabNames: []string{"web"}},
		{Action: allow, LabNames: []string{"forensics"}},
		{Action: deny, ClientNames: []string{"alice"}, LabNames: []string{"forensics"}},
	}
	res, err := h.SetLabGroupAccess(ctx, policy(rules...))
	wantStates(t, res, err, stCreated)
	stored, err := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(namespace).Get(ctx, "access-policy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get group policy: %v", err)
	}
	if len(stored.Spec.Rules) != 3 || stored.Spec.Rules[1].Action != laboratoryv1alpha1.LabGroupAccessAllow || len(stored.Spec.Rules[1].ClientNames) != 0 {
		t.Fatalf("stored rules = %#v", stored.Spec.Rules)
	}
	if stored.Labels["event"] != "e1" || stored.Labels["scope"] != "access" {
		t.Fatalf("policy labels: %v", stored.Labels)
	}

	res, err = h.SetLabGroupAccess(ctx, policy(rules...))
	wantStates(t, res, err, stExists)
	res, err = h.SetLabGroupAccess(ctx, policy(rules[0]))
	wantStates(t, res, err, stUpdated)

	// An unknown lab is refused without a partial write, and does not fail the other policies.
	bad := policy(&protobuf.LabGroupAccessRule{Action: allow, LabNames: []string{"missing"}})
	bad.Policies = append(bad.Policies, &protobuf.LabGroupAccessPolicy{LabGroupName: "no-such-group"})
	res, err = h.SetLabGroupAccess(ctx, bad)
	wantStates(t, res, err, stFailed, stNotFound)
	stored, _ = h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(namespace).Get(ctx, "access-policy", metav1.GetOptions{})
	if len(stored.Spec.Rules) != 1 {
		t.Fatalf("a refused policy changed the stored one: %#v", stored.Spec.Rules)
	}
}
