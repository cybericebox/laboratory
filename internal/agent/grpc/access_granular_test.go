package grpc

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// Access per client and per connector: the agent checks the connectors (the VPN gateways of the named labs) and the include entries.
func TestSetLabGroupAccessGranular(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	const namespace = "team-granular"
	readyGroup(t, h, k8s, "granular", namespace, nil)
	gateways := []laboratoryv1alpha1.GatewayTemplate{
		{Name: "office", Kind: laboratoryv1alpha1.GatewayKindVPN},
		{Name: "dmz", Kind: laboratoryv1alpha1.GatewayKindVPN},
		{Name: "out", Kind: laboratoryv1alpha1.GatewayKindInternet},
	}
	if _, err := h.cs.LaboratoryV1alpha1().Labs(namespace).Create(ctx, &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: namespace}, Spec: laboratoryv1alpha1.LabSpec{Gateways: gateways}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	allow, deny := protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW, protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_DENY
	set := func(r *protobuf.LabGroupAccessRule) protobuf.ItemState {
		res, err := h.SetLabGroupAccess(ctx, &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{{LabGroupName: "granular", Rules: []*protobuf.LabGroupAccessRule{r}}}})
		if err != nil || len(res.Results) != 1 {
			t.Fatalf("set: %v %v", res, err)
		}
		return res.Results[0].State
	}
	inc := func(host, prefix int32) *protobuf.AccessInclude {
		return &protobuf.AccessInclude{Host: host, Prefix: prefix}
	}

	ok := &protobuf.LabGroupAccessRule{Action: allow, ClientNames: []string{"alice"}, LabNames: []string{"web"}, GatewayNames: []string{"dmz", "office", "dmz"},
		Include: []*protobuf.AccessInclude{inc(10, 32), inc(64, 26), inc(10, 32)}}
	if st := set(ok); st != stCreated {
		t.Fatalf("valid granular rule: %v", st)
	}
	stored, err := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(namespace).Get(ctx, "access-policy", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := stored.Spec.Rules[0]
	if len(r.GatewayNames) != 2 || r.GatewayNames[0] != "dmz" || r.GatewayNames[1] != "office" || len(r.Include) != 2 || r.Include[0] != (laboratoryv1alpha1.AccessInclude{Host: 10, Prefix: 32}) {
		t.Fatalf("stored rule = %#v (gateways sorted without duplicates, include without duplicates)", r)
	}

	for name, rule := range map[string]*protobuf.LabGroupAccessRule{
		"allow without labs":            {Action: allow, GatewayNames: []string{"office"}},
		"gateway of another kind":       {Action: allow, LabNames: []string{"web"}, GatewayNames: []string{"out"}},
		"gateway the lab does not have": {Action: allow, LabNames: []string{"web"}, GatewayNames: []string{"missing"}},
		"include without gateways":      {Action: allow, LabNames: []string{"web"}, Include: []*protobuf.AccessInclude{inc(10, 32)}},
		"include on a deny rule":        {Action: deny, GatewayNames: []string{"office"}, Include: []*protobuf.AccessInclude{inc(10, 32)}},
		"prefix wider than the /24":     {Action: allow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []*protobuf.AccessInclude{inc(0, 16)}},
		"prefix longer than 32":         {Action: allow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []*protobuf.AccessInclude{inc(0, 33)}},
		"host outside the segment":      {Action: allow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []*protobuf.AccessInclude{inc(256, 32)}},
		"host not at the subnet start":  {Action: allow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []*protobuf.AccessInclude{inc(5, 29)}},
		"nil include entry":             {Action: allow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []*protobuf.AccessInclude{nil}},
	} {
		if st := set(rule); st != stFailed {
			t.Errorf("%s: state %v, want failed", name, st)
		}
	}
	stored, _ = h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(namespace).Get(ctx, "access-policy", metav1.GetOptions{})
	if len(stored.Spec.Rules) != 1 || len(stored.Spec.Rules[0].Include) != 2 {
		t.Fatalf("a refused rule changed the stored policy: %#v", stored.Spec.Rules)
	}
}
