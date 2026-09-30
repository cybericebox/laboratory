package l7

import (
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// TokenMode says how strictly tokens must name a user.
type TokenMode string

const (
	// ModeLegacy is the old behaviour: any token of the group reaches every
	// device of the group and nothing is counted per user.
	ModeLegacy TokenMode = "legacy"
	// ModeMixed accepts per-user tokens (counted, and checked against explicit
	// deny rules of the group policy) and old tokens (not counted).
	ModeMixed TokenMode = "mixed"
	// ModePerUser requires a user in the token and applies the same access
	// policy the VPN uses: a lab is reachable only when the policy allows it for
	// this participant, so a revoked participant loses the web at once.
	ModePerUser TokenMode = "per-user"
)

func (m TokenMode) Valid() bool { return m == ModeLegacy || m == ModeMixed || m == ModePerUser }

// ClientName is the LabGroupClient name of a participant.
func ClientName(subject string) string { return "p-" + subject }

// PolicyAllows evaluates the group access policy for one client and lab the way
// the VPN does: a matching deny wins; without a matching allow the pair is
// denied by default. When requireAllow is false only explicit denies apply.
func PolicyAllows(rules []laboratoryv1alpha1.LabGroupAccessRule, client, lab string, requireAllow bool) bool {
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
	return allowed || !requireAllow
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
