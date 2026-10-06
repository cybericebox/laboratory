// Package errorlog is the laboratory's own error journal: every component (operator, node-agent, L7 proxy, wg-demux, VPN and
// gateway pods, the agent) counts its errors by a normalized fingerprint, keeps a few recent messages and publishes the aggregate
// as a Kubernetes Event that the management agent reads and forwards in MonitoringUpdate.errors. Nothing here may carry tenant or
// participant data, an address or a secret: every message passes Redact before it is stored, and the fingerprint is made from the
// further Normalize d text.
package errorlog

import (
	"regexp"
	"strings"
)

const (
	// MaxSampleLen is the longest message kept.
	MaxSampleLen = 300
)

var scrubbers = []struct {
	re   *regexp.Regexp
	with string
}{
	// key material and tokens first, before anything breaks them up
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+-----.*?(-----END [A-Z ]+-----|$)`), "<pem>"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), "<jwt>"},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 <redacted>"},
	// credentials in URLs
	{regexp.MustCompile(`([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`), "${1}<redacted>@"},
	// key=value and "key":"value" of secret-looking keys
	{regexp.MustCompile(`(?i)((?:pass(?:word|wd)?|token|secret|api[-_]?key|access[-_]?key|private[-_]?key|authorization|credential[s]?|cookie|session)["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}&]+)`), "${1}<redacted>"},
	// email addresses
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "<email>"},
	// IPv6 (before IPv4 so ::ffff:1.2.3.4 is one match), then IPv4 with an optional port or prefix
	{regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{0,4}(?:/\d{1,3})?|::(?:[0-9a-f]{1,4}:?){1,7}\b`), "<ip>"},
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{1,5})?(?:/\d{1,2})?\b`), "<ip>"},
	// MAC addresses
	{regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`), "<mac>"},
	// UUIDs and ULIDs (platform ids of groups and labs look like these)
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<id>"},
	{regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`), "<id>"},
	// long hex or base64-ish blobs (digests, keys, tokens)
	{regexp.MustCompile(`(?i)\b(?:sha256:)?[0-9a-f]{24,}\b`), "<hash>"},
	{regexp.MustCompile(`\b[A-Za-z0-9+/_-]{32,}={0,2}`), "<blob>"},
	// host names (registries, services, pods' DNS names, tenants' hosts)
	{regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.){2,}[a-z]{2,}(?::\d{1,5})?\b`), "<host>"},
	// the namespaces and names of the platform's objects: group namespaces, namespace/name pairs
	{regexp.MustCompile(`\blg-[a-z0-9-]+\b`), "<ns>"},
	{regexp.MustCompile(`\b[a-z0-9][a-z0-9.-]*/[a-z0-9][a-z0-9._-]*\b`), "<obj>"},
	// quoted strings (names of tenants, groups, labs, images)
	{regexp.MustCompile(`"[^"]{1,200}"`), `"<str>"`},
	{regexp.MustCompile("`[^`]{1,200}`"), "`<str>`"},
}

// Redact removes what must not leave the cluster from an error message: key material, tokens, passwords, addresses (IPv4, IPv6,
// MAC), email addresses, host names, the names of objects and any quoted string (they carry tenant and group names), and shortens it
// to MaxSampleLen. What is left says what failed, not for whom.
func Redact(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	for _, s := range scrubbers {
		msg = s.re.ReplaceAllString(msg, s.with)
	}
	if len(msg) > MaxSampleLen {
		msg = msg[:MaxSampleLen-3] + "..."
	}
	return msg
}

var (
	digits     = regexp.MustCompile(`\d+`)
	hexRun     = regexp.MustCompile(`(?i)\b[0-9a-f]{8,}\b`)
	podSuffix  = regexp.MustCompile(`\b([a-z][a-z0-9]*(?:-[a-z0-9]+)*)-[a-z0-9]{5,10}-[a-z0-9]{5}\b`)
	manySpaces = regexp.MustCompile(`\s+`)
)

// Normalize is Redact with the parts that vary between occurrences of the same error replaced, so that the occurrences group
// together: numbers become <n>, long hex runs <id>, generated pod names lose their suffix. The fingerprint is made from it.
func Normalize(msg string) string {
	msg = Redact(msg)
	msg = podSuffix.ReplaceAllString(msg, "$1-<id>")
	msg = hexRun.ReplaceAllString(msg, "<id>")
	msg = digits.ReplaceAllString(msg, "<n>")
	return manySpaces.ReplaceAllString(msg, " ")
}
