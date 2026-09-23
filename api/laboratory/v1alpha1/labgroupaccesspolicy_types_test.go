package v1alpha1

import "testing"

func TestLabGroupAccessPolicyDefaultsToDeny(t *testing.T) {
	policy := LabGroupAccessPolicy{}
	if len(policy.Spec.Rules) != 0 {
		t.Fatalf("rules = %#v, want an empty default-deny policy", policy.Spec.Rules)
	}
}

func TestLabGroupAccessRuleAllowsAllClientsWhenClientNamesAreEmpty(t *testing.T) {
	rule := LabGroupAccessRule{Action: LabGroupAccessAllow, LabNames: []string{"web"}}
	if len(rule.ClientNames) != 0 || rule.Action != LabGroupAccessAllow {
		t.Fatalf("rule = %#v", rule)
	}
}
