package names

import (
	"strings"
	"testing"
)

func TestAccessHostLabels(t *testing.T) {
	if l, ok := AccessHostLabel("kali", "x7k", AccessKindHTTP); !ok || l != "kali-x7k" || l != WebHostLabel("kali", "x7k") {
		t.Fatalf("http label %q", l)
	}
	if l, ok := AccessHostLabel("kali", "x7k", AccessKindTerminal); !ok || l != "kali-x7k--term" {
		t.Fatalf("terminal label %q", l)
	}
	if _, ok := AccessHostLabel("kali", "x7k", "ssh"); ok {
		t.Fatal("an unknown kind has a label")
	}
	long := strings.Repeat("a", MaxDeviceNameLen)
	for _, kind := range []string{AccessKindHTTP, AccessKindTerminal, AccessKindVNC, AccessKindRDP} {
		if l, _ := AccessHostLabel(long, "abcd", kind); len(l) > 63 {
			t.Fatalf("%s label is %d characters", kind, len(l))
		}
	}
}

func TestAccessHostParse(t *testing.T) {
	for _, c := range []struct{ label, base, kind string }{
		{"kali-x7k", "kali-x7k", AccessKindHTTP},
		{"kali-x7k--term", "kali-x7k", AccessKindTerminal},
		{"kali-x7k--vnc", "kali-x7k", AccessKindVNC},
		{"kali-x7k--rdp", "kali-x7k", AccessKindRDP},
		{"a--b-x7k", "a--b-x7k", AccessKindHTTP},
		{"term-x7k", "term-x7k", AccessKindHTTP},
		{"web--term-x7k", "web--term-x7k", AccessKindHTTP},
	} {
		if base, kind := ParseAccessHost(c.label); base != c.base || kind != c.kind {
			t.Errorf("%q: %q %q", c.label, base, kind)
		}
	}
}

// No pair (device, code) gives a web host that reads as a kind host, and no kind host reads as a web host: the web host ends in
// "-<code>" (3 or 4 letters or digits), the kind host in "--<marker>".
func TestAccessHostsNeverCollide(t *testing.T) {
	alphabet := "ab0-"
	var devices []string
	var gen func(prefix string, n int)
	gen = func(prefix string, n int) {
		if prefix != "" && ValidateDeviceName(prefix) == nil {
			devices = append(devices, prefix)
		}
		if n == 0 {
			return
		}
		for _, c := range alphabet {
			gen(prefix+string(c), n-1)
		}
	}
	gen("", 6)
	codes := []string{"abc", "a0b", "0ab", "b00", "abcd", "0000", "ab0b"}
	seen := map[string]string{}
	for _, d := range devices {
		for _, code := range codes {
			for _, kind := range []string{AccessKindHTTP, AccessKindTerminal, AccessKindVNC, AccessKindRDP} {
				l, _ := AccessHostLabel(d, code, kind)
				base, got := ParseAccessHost(l)
				if got != kind || base != WebHostLabel(d, code) {
					t.Fatalf("%s %s %s -> %q parses as %q %q", d, code, kind, l, base, got)
				}
				who := d + "/" + code + "/" + kind
				if other, dup := seen[l]; dup {
					// the same label for two different (device, code) of the same kind is a plain web collision the operator already prevents
					if other != who && strings.HasSuffix(other, "/"+kind) {
						continue
					}
					t.Fatalf("%q is both %s and %s", l, other, who)
				}
				seen[l] = who
			}
		}
	}
}

func TestDeviceNameMayNotLookLikeAnAccessHost(t *testing.T) {
	for _, n := range []string{"box--term", "box--vnc", "x--rdp"} {
		if ValidateNewDeviceName(n) == nil {
			t.Errorf("%q accepted", n)
		}
	}
	if err := ValidateNewDeviceName("terminal"); err != nil {
		t.Errorf("an ordinary name is refused: %v", err)
	}
}
