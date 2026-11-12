package l7

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Embedding the console page in another site's page. Two lists decide, and nothing else: an origin that matches an `allow` entry and no
// `deny` entry (deny wins) may frame the console; every other page may not. An entry is `https://host[:port]`, or `https://*.base[:port]`
// where `*` is exactly one label, leftmost only. Nothing is implied: the labs' own domain is not excluded unless the list says so.
//
// CSP cannot say "one label", so the header is made per response: the origin of the Referer of the page request (a browser sends it for a
// frame navigation, as an origin) is matched here, and the header names that one origin. The browser then checks it against the real
// parent: a forged Referer only produces a header that blocks the forger. Empty allow list, no Referer, an unparsable one or no match:
// `frame-ancestors 'none'`.

var (
	labelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	portRE  = regexp.MustCompile(`^[0-9]{1,5}$`)
)

type originPattern struct {
	wildcard bool
	host     string // the host, or for a wildcard the base after "*."
	port     string // empty = the default port
}

func parseOriginPattern(entry string) (originPattern, error) {
	rest, ok := strings.CutPrefix(entry, "https://")
	if !ok || rest == "" {
		return originPattern{}, fmt.Errorf("%q: the entry must start with https://", entry)
	}
	if strings.ContainsAny(rest, "/?#@\\ ") {
		return originPattern{}, fmt.Errorf("%q: no path, query, user or space in an entry", entry)
	}
	host, port, hasPort := strings.Cut(rest, ":")
	if hasPort && !portRE.MatchString(port) {
		return originPattern{}, fmt.Errorf("%q: bad port", entry)
	}
	if port == "443" {
		port = ""
	}
	p := originPattern{port: port}
	if base, ok := strings.CutPrefix(host, "*."); ok {
		p.wildcard, host = true, base
		// a base of one label (*.com) would be every site of a suffix
		if strings.Count(host, ".") < 1 {
			return originPattern{}, fmt.Errorf("%q: a wildcard needs a base of at least two labels", entry)
		}
	}
	if host != strings.ToLower(host) {
		return originPattern{}, fmt.Errorf("%q: use lower case", entry)
	}
	for _, l := range strings.Split(host, ".") {
		if !labelRE.MatchString(l) {
			return originPattern{}, fmt.Errorf("%q: %q is not a DNS label (a * is allowed only as the whole leftmost label)", entry, l)
		}
	}
	p.host = host
	return p, nil
}

func (p originPattern) matches(host, port string) bool {
	if p.port != port {
		return false
	}
	if !p.wildcard {
		return host == p.host
	}
	label, base, ok := strings.Cut(host, ".")
	return ok && base == p.host && labelRE.MatchString(label)
}

// EmbedMatcher is the parsed allow and deny lists. It is built once at start and read-only after that.
type EmbedMatcher struct{ allow, deny []originPattern }

// NewEmbedMatcher parses the lists; a malformed entry is an error.
func NewEmbedMatcher(allow, deny []string) (*EmbedMatcher, error) {
	m := &EmbedMatcher{}
	for _, e := range allow {
		p, err := parseOriginPattern(strings.TrimSpace(e))
		if err != nil {
			return nil, fmt.Errorf("embedding allow: %w", err)
		}
		m.allow = append(m.allow, p)
	}
	for _, e := range deny {
		p, err := parseOriginPattern(strings.TrimSpace(e))
		if err != nil {
			return nil, fmt.Errorf("embedding deny: %w", err)
		}
		m.deny = append(m.deny, p)
	}
	return m, nil
}

const ancestorNone = "'none'"

// Ancestor is what `frame-ancestors` gets for a request with this Referer: the Referer's origin when the lists allow it, else 'none'.
func (m *EmbedMatcher) Ancestor(referer string) string {
	if m == nil || len(m.allow) == 0 || referer == "" {
		return ancestorNone
	}
	u, err := url.Parse(referer)
	if err != nil || u.Scheme != schemeHTTPS || u.Host == "" || u.User != nil {
		return ancestorNone
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "443" {
		port = ""
	}
	// only a plain DNS name can be written into the header
	for _, l := range strings.Split(host, ".") {
		if !labelRE.MatchString(l) {
			return ancestorNone
		}
	}
	allowed := false
	for _, p := range m.allow {
		if p.matches(host, port) {
			allowed = true
			break
		}
	}
	if !allowed {
		return ancestorNone
	}
	for _, p := range m.deny {
		if p.matches(host, port) {
			return ancestorNone
		}
	}
	origin := "https://" + host
	if port != "" {
		origin += ":" + port
	}
	return origin
}
