package egressproxy

import (
	"net/netip"
	"strings"
)

// Why a connection is refused. The words are the labels of the summaries and of the log.
const (
	ReasonNoName   = "no-name"   // no SNI / no Host
	ReasonIPName   = "ip-name"   // the name is an IP address (literal, decimal, hex, bracketed)
	ReasonBadName  = "bad-name"  // not a host name
	ReasonECH      = "ech"       // the ClientHello hides its name
	ReasonDenied   = "tunnel"    // a known tunnel service
	ReasonResolve  = "resolve"   // the name does not resolve to a public address
	ReasonHostRate = "host-rate" // too many new connections to one name
	ReasonLabRate  = "lab-rate"  // the lab is at its caps
	ReasonNotTLS   = "not-tls"
	ReasonNotHTTP  = "not-http"
	ReasonUnknown  = "unknown-lab"
)

// DefaultTunnelSuffixes are the names of services whose purpose is to carry a lab's traffic somewhere else (a reverse tunnel out, a VPN
// control plane). A name equal to an entry or under it is refused. The list is configurable and small on purpose: the web is open.
var DefaultTunnelSuffixes = []string{
	"ngrok.io", "ngrok.com", "ngrok.app", "ngrok-free.app", "ngrok-free.dev", "ngrok.dev", "ngrok.pizza", "ngrok.pro",
	"trycloudflare.com", "loca.lt", "localtunnel.me", "serveo.net", "localhost.run", "lhr.life", "pinggy.io", "pinggy.link", "pagekite.me",
	"tailscale.com", "tailscale.io", "ts.net", "zerotier.com",
}

// NormalizeName lower-cases a name, drops the trailing dot and any port, and says whether it may be looked up: not an IP address in any
// notation, not empty, made of host-name labels. The reason is one of the Reason* words.
func NormalizeName(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ReasonNoName
	}
	if strings.HasPrefix(s, "[") { // [v6] or [v6]:port
		return "", ReasonIPName
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		if strings.Contains(s[:i], ":") { // a bare IPv6 address
			return "", ReasonIPName
		}
		s = s[:i]
	}
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if s == "" {
		return "", ReasonNoName
	}
	if len(s) > 253 {
		return "", ReasonBadName
	}
	if _, err := netip.ParseAddr(s); err == nil || ipLikeLabel(s) {
		return "", ReasonIPName
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ReasonBadName
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return "", ReasonBadName
			}
		}
	}
	return s, ""
}

// ipLikeLabel is true when the last label of a name is a number (decimal, 0x hex, octal): the libc forms of an IPv4 address ("2130706433",
// "0x7f.1", "127.1") that netip does not parse. A real top-level domain is never numeric.
func ipLikeLabel(s string) bool {
	last := s
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		last = s[i+1:]
	}
	if last == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") {
		return true
	}
	for _, c := range last {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Denylist is the set of tunnel-service suffixes.
type Denylist struct{ suffixes []string }

// NewDenylist lower-cases the suffixes; an empty list denies nothing.
func NewDenylist(suffixes []string) *Denylist {
	d := &Denylist{}
	for _, s := range suffixes {
		if s = strings.ToLower(strings.Trim(strings.TrimSpace(s), ".")); s != "" {
			d.suffixes = append(d.suffixes, s)
		}
	}
	return d
}

// Denied says a normalized name is one of the suffixes or under one.
func (d *Denylist) Denied(name string) bool {
	if d == nil {
		return false
	}
	for _, s := range d.suffixes {
		if name == s || strings.HasSuffix(name, "."+s) {
			return true
		}
	}
	return false
}
