package registrygate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/cybericebox/laboratory/internal/netguard"
)

// maxProofRedirects is how many redirects of a manifest proof are followed (fixed, not a setting).
const maxProofRedirects = 3

// ParseRedirectPatterns checks and normalizes the hosts a registry's manifest requests may be redirected to: an exact host
// ("cdn.example.com") or one leading "*." for subdomains only ("*.pkg.dev"). Refused: an empty list entry, a bare "*", a "*" anywhere else,
// anything that is not a plain host name (a scheme, port, path, userinfo, an IP literal, an empty label, a trailing dot), and a pattern
// whose host part is a public suffix itself ("*.com", "*.co.uk", "dev"): it would name every owner under it. The gate refuses to start
// with a list it cannot enforce.
func ParseRedirectPatterns(patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		n, err := parseRedirectPattern(p)
		if err != nil {
			return nil, fmt.Errorf("redirect pattern %q: %w", p, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func parseRedirectPattern(p string) (string, error) {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "", errors.New("empty")
	}
	host := strings.TrimPrefix(p, "*.")
	if strings.Contains(host, "*") {
		return "", errors.New(`only one leading "*." wildcard is allowed`)
	}
	if !netguard.PlainHostName(host) {
		return "", errors.New("not a plain host name (no scheme, port, path, userinfo, IP address, empty label or trailing dot)")
	}
	if net.ParseIP(host) != nil {
		return "", errors.New("an IP address is not a host name")
	}
	if suffix, _ := publicsuffix.PublicSuffix(host); suffix == host {
		return "", errors.New("a public suffix alone (it would match unrelated owners)")
	}
	if p != host { // "*.h": a subdomain of h that is itself a public suffix (a wildcard rule, "*.compute.amazonaws.com") names every owner under it
		if suffix, _ := publicsuffix.PublicSuffix("x." + host); suffix == "x."+host {
			return "", errors.New("its subdomains are public suffixes (they would match unrelated owners)")
		}
	}
	if l := host[strings.LastIndexByte(host, '.')+1:]; strings.Trim(l, "0123456789") == "" {
		return "", errors.New("a numeric last label (an IPv4 address in a short form), not a host name")
	}
	return p, nil
}

// redirectHostMatches says whether host is an exact pattern or a subdomain of a wildcard one. The patterns are normalized.
func redirectHostMatches(patterns []string, host string) bool {
	host = strings.ToLower(host)
	for _, p := range patterns {
		if rest, wild := strings.CutPrefix(p, "*."); wild {
			if strings.HasSuffix(host, "."+rest) && len(host) > len(rest)+1 {
				return true
			}
		} else if host == p {
			return true
		}
	}
	return false
}

// redirectTarget resolves the Location of a redirect against the URL that was requested and says whether it may be followed for the
// registry: https, no userinfo, no port (the default one), a host name (not an IP literal) matching one of the registry's patterns.
func redirectTarget(reg Registry, resp *http.Response) (*url.URL, bool) {
	if len(reg.Redirects) == 0 || resp.Request == nil || resp.Request.URL == nil {
		return nil, false
	}
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return nil, false
	}
	ref, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.Header.Get("Location") == "" {
		return nil, false
	}
	t := resp.Request.URL.ResolveReference(ref)
	host := t.Hostname()
	if t.Scheme != schemeHTTPS || t.User != nil || t.Port() != "" || t.Host != host || host == "" || strings.HasSuffix(host, ".") ||
		net.ParseIP(host) != nil || !netguard.PlainHostName(strings.ToLower(host)) || !redirectHostMatches(reg.Redirects, host) {
		return nil, false
	}
	t.Fragment = ""
	return t, true
}

// followProof follows the redirects of a manifest proof that the registry's list allows, at most maxProofRedirects, each one checked
// again against the ORIGINAL registry's patterns. A request to the target carries the method and the Accept header and nothing else: never
// the Authorization of the registry (which is sent only to the registry's own URL and its token endpoint), no body. The caller bounds the whole chain with one deadline (Upstream.Timeout). It returns the last response, or ok false when a redirect was not allowed or the chain was too
// long (the answer is then inconclusive).
func (u *Upstream) followProof(ctx context.Context, reg Registry, resp *http.Response, method string, o Object) (*http.Response, bool, error) {
	for hops := 0; ; hops++ {
		t, ok := redirectTarget(reg, resp)
		if !ok {
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				drain(resp)
				return nil, false, nil
			}
			return resp, true, nil
		}
		drain(resp)
		if hops >= maxProofRedirects {
			return nil, false, nil
		}
		req, err := http.NewRequestWithContext(netguard.HopContext(ctx), method, t.String(), nil) // dialed with the internal addresses refused
		if err != nil {
			return nil, false, err
		}
		if o.Accept != "" {
			req.Header.Set("Accept", o.Accept)
		}
		if resp, err = u.HTTP.Do(req); err != nil {
			return nil, false, err
		}
	}
}
