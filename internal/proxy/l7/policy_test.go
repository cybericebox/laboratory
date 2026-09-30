package l7

import (
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestPolicyAllowsFollowsTheVPNRules(t *testing.T) {
	allow := func(clients, labs []string) laboratoryv1alpha1.LabGroupAccessRule {
		return laboratoryv1alpha1.LabGroupAccessRule{Action: laboratoryv1alpha1.LabGroupAccessAllow, ClientNames: clients, LabNames: labs}
	}
	deny := func(clients, labs []string) laboratoryv1alpha1.LabGroupAccessRule {
		return laboratoryv1alpha1.LabGroupAccessRule{Action: laboratoryv1alpha1.LabGroupAccessDeny, ClientNames: clients, LabNames: labs}
	}
	rules := []laboratoryv1alpha1.LabGroupAccessRule{
		allow([]string{"p-a"}, []string{"c-1", "c-2"}),
		allow([]string{"p-b"}, []string{"c-1"}),
		deny([]string{"p-b"}, nil),
	}
	cases := []struct {
		name        string
		client, lab string
		want        bool
	}{
		{"allowed pair", "p-a", "c-2", true},
		{"other lab is denied by default", "p-a", "c-3", false},
		{"unknown client is denied by default", "p-z", "c-1", false},
		{"a deny beats an allow", "p-b", "c-1", false},
	}
	for _, c := range cases {
		if got := PolicyAllows(rules, c.client, c.lab); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if PolicyAllows(nil, "p-a", "c-1") {
		t.Error("an empty policy is closed")
	}
}
