package l7

import (
	"strings"
	"testing"
)

func matcher(t *testing.T, allow, deny []string) *EmbedMatcher {
	t.Helper()
	m, err := NewEmbedMatcher(allow, deny)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEmbedAncestor(t *testing.T) {
	exact := matcher(t, []string{"https://exercises.example.com", "https://academy.example.org:8443"}, nil)
	wild := matcher(t, []string{"https://*.example.com"}, []string{"https://vpn.example.com", "https://labs.example.com"})
	none := ancestorNone
	for name, c := range map[string]struct {
		m       *EmbedMatcher
		referer string
		want    string
	}{
		"exact entry":                  {exact, "https://exercises.example.com/some/page?x=1", "https://exercises.example.com"},
		"exact entry with a port":      {exact, "https://academy.example.org:8443/", "https://academy.example.org:8443"},
		"exact entry, other port":      {exact, "https://academy.example.org/", none},
		"exact entry, default port":    {exact, "https://exercises.example.com:443/x", "https://exercises.example.com"},
		"other host":                   {exact, "https://evil.example.com/", none},
		"host in upper case":           {exact, "https://EXERCISES.example.com/", "https://exercises.example.com"},
		"suffix trick":                 {exact, "https://exercises.example.com.evil.net/", none},
		"prefix trick":                 {exact, "https://evilexercises.example.com/", none},
		"user info":                    {exact, "https://exercises.example.com@evil.net/", none},
		"http scheme":                  {exact, "http://exercises.example.com/", none},
		"missing referer":              {exact, "", none},
		"unparsable":                   {exact, "%%%not a url", none},
		"not a url":                    {exact, "exercises.example.com", none},
		"wildcard, one label":          {wild, "https://a.example.com/", "https://a.example.com"},
		"wildcard, two labels":         {wild, "https://x.labs.example.com/", none},
		"wildcard, the base itself":    {wild, "https://example.com/", none},
		"wildcard, deny wins (vpn)":    {wild, "https://vpn.example.com/", none},
		"wildcard, deny wins (labs)":   {wild, "https://labs.example.com/", none},
		"wildcard, a labs subdomain":   {wild, "https://kali-ab12.labs.example.com/", none},
		"wildcard, http scheme":        {wild, "http://a.example.com/", none},
		"wildcard, other base":         {wild, "https://a.example.org/", none},
		"wildcard, a header injection": {wild, "https://a.example.com;%20script-src%20*/", none},
		"empty allow list":             {matcher(t, nil, nil), "https://a.example.com/", none},
		"deny without allow":           {matcher(t, nil, []string{"https://a.example.com"}), "https://a.example.com/", none},
		"no matcher":                   {nil, "https://a.example.com/", none},
	} {
		if got := c.m.Ancestor(c.referer); got != c.want {
			t.Errorf("%s: %q -> %q, want %q", name, c.referer, got, c.want)
		}
	}
	// a deny list that names nothing relevant changes nothing
	if got := matcher(t, []string{"https://a.example.com"}, []string{"https://b.example.com"}).Ancestor("https://a.example.com/"); got != "https://a.example.com" {
		t.Errorf("unrelated deny: %q", got)
	}
}

func TestEmbedEntriesAreValidated(t *testing.T) {
	for _, bad := range []string{"", "*", "https://*", "https://*.com", "https://a.*.example.com", "https://**.example.com", "https://a*.example.com", "http://a.example.com",
		"a.example.com", "https://a.example.com/path", "https://a.example.com/", "https://a.example.com?x", "https://u@a.example.com", "https://A.example.com",
		"https://a.example.com:port", "https://a_b.example.com", "*.example.com", "https://*.example.com/x"} {
		if _, err := NewEmbedMatcher([]string{bad}, nil); err == nil {
			t.Errorf("allow entry %q accepted", bad)
		}
		if _, err := NewEmbedMatcher(nil, []string{bad}); err == nil && strings.TrimSpace(bad) == bad {
			t.Errorf("deny entry %q accepted", bad)
		}
	}
	for _, ok := range []string{"https://a.example.com", "https://*.example.com", "https://a.example.com:8443", "https://localhost", "https://*.sub.example.com"} {
		if _, err := NewEmbedMatcher([]string{ok}, []string{ok}); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}

// The page gets the header per response; the other answers always forbid framing; top-level navigation works with or without a Referer.
func TestTerminalPageFrameAncestors(t *testing.T) {
	f := newTermFixture(t, func(c *TerminalConfig) {
		c.Embed = matcher(t, []string{"https://*.example.com"}, []string{"https://vpn.example.com"})
	})
	ancestors := func(path string, referer string) (int, string, string) {
		hdr := map[string]string{}
		if referer != "" {
			hdr["Referer"] = referer
		}
		rec := f.get(path, hdr)
		csp := rec.Header().Get("Content-Security-Policy")
		i := strings.Index(csp, "frame-ancestors ")
		return rec.Code, strings.TrimSpace(csp[i+len("frame-ancestors "):]), rec.Header().Get("Vary")
	}
	for referer, want := range map[string]string{
		"https://exercises.example.com/catalog": "https://exercises.example.com",
		"https://vpn.example.com/":              "'none'",
		"https://x.y.example.com/":              "'none'",
		"http://exercises.example.com/":         "'none'",
		"":                                      "'none'",
	} {
		code, got, vary := ancestors("/?lab=lab1&device=kali", referer)
		if code != 200 {
			t.Errorf("referer %q: the page did not load as a top-level navigation: %d", referer, code)
		}
		if got != want {
			t.Errorf("referer %q: frame-ancestors %q, want %q", referer, got, want)
		}
		if !strings.Contains(vary, "Referer") {
			t.Errorf("referer %q: Vary = %q", referer, vary)
		}
	}
	if rec := f.get("/?lab=lab1&device=kali", map[string]string{"Referer": "https://a.example.com/"}); rec.Header().Get("X-Frame-Options") != "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v", rec.Header())
	}
	// the assets are never offered to a frame
	if _, got, _ := ancestors("/assets/terminal.js", "https://a.example.com/"); got != "'none'" {
		t.Errorf("an asset: %q", got)
	}
	// without any list the default is no embedding at all
	g := newTermFixture(t, nil)
	rec := g.get("/?lab=lab1&device=kali", map[string]string{"Referer": "https://a.example.com/"})
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("default: %s", csp)
	}
}
