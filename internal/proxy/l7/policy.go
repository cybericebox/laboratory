package l7

import (
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// PolicyAllows evaluates the group access policy for one client and lab the way
// the VPN does: a matching deny wins; without a matching allow the pair is
// denied by default.
func PolicyAllows(rules []laboratoryv1alpha1.LabGroupAccessRule, client, lab string) bool {
	allowed := false
	for _, rule := range rules {
		if !matches(rule.ClientNames, client) || !matches(rule.LabNames, lab) {
			continue
		}
		switch rule.Action {
		case laboratoryv1alpha1.LabGroupAccessDeny:
			return false
		case laboratoryv1alpha1.LabGroupAccessAllow:
			allowed = true
		}
	}
	return allowed
}

// matches: an empty selector means everything.
func matches(selector []string, value string) bool {
	if len(selector) == 0 {
		return true
	}
	for _, s := range selector {
		if s == value {
			return true
		}
	}
	return false
}
