package names

import (
	"strings"
	"testing"
)

func TestNewWebCodeIsABaseCode(t *testing.T) {
	seen := map[byte]bool{}
	for i := 0; i < 2000; i++ {
		for _, n := range []int{WebCodeLen, WebCodeMaxLen} {
			c, err := NewWebCode(n)
			if err != nil || len(c) != n || !IsBaseCode(c) {
				t.Fatalf("%q %v", c, err)
			}
			seen[c[len(c)-1]] = true
		}
	}
	// the six last characters, and nothing else, are drawn
	if len(seen) != KindSlots {
		t.Fatalf("last characters %v, want the six of index 0 mod 6", seen)
	}
	for _, c := range "06ciou" {
		if !seen[byte(c)] {
			t.Errorf("%q never drawn", c)
		}
	}
}

func TestAccessHostLabels(t *testing.T) {
	for _, c := range []struct{ code, kind, want string }{
		{"ab0", AccessKindHTTP, "kali-ab0"},
		{"ab0", AccessKindTerminal, "kali-ab1"},
		{"ab0", AccessKindVNC, "kali-ab2"},
		{"ab0", AccessKindRDP, "kali-ab3"},
		{"abu", AccessKindTerminal, "kali-abv"},
		{"abc", AccessKindRDP, "kali-abf"},
		{"ab6", AccessKindHTTP, "kali-ab6"},
	} {
		got, ok := AccessHostLabel("kali", c.code, c.kind)
		if !ok || got != c.want {
			t.Errorf("%s %s: %q %v, want %q", c.code, c.kind, got, ok, c.want)
		}
	}
	if l, _ := AccessHostLabel("kali", "x7c", AccessKindHTTP); l != WebHostLabel("kali", "x7c") {
		t.Errorf("http label %q differs from the web host label", l)
	}
	if _, ok := AccessHostLabel("kali", "x7k", AccessKindTerminal); ok {
		t.Error("a code that is not a base code has an access host")
	}
	if _, ok := AccessHostLabel("kali", "x7c", "ssh"); ok {
		t.Error("an unknown kind has a label")
	}
	long := strings.Repeat("a", MaxDeviceNameLen)
	for _, kind := range []string{AccessKindHTTP, AccessKindTerminal, AccessKindVNC, AccessKindRDP} {
		if l, _ := AccessHostLabel(long, "abc0", kind); len(l) > 63 {
			t.Fatalf("%s label is %d characters", kind, len(l))
		}
	}
}

// Over every code of length 3 and every kind: the label parses back to the device, the base code and the kind; no two (base code,
// kind) give the same label; the http label is the web host label.
func TestAccessHostsExhaustive(t *testing.T) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	kinds := []string{AccessKindHTTP, AccessKindTerminal, AccessKindVNC, AccessKindRDP}
	seen := map[string]string{}
	bases := 0
	for i := 0; i < len(alphabet); i++ {
		for j := 0; j < len(alphabet); j++ {
			for k := 0; k < len(alphabet); k++ {
				code := string([]byte{alphabet[i], alphabet[j], alphabet[k]})
				if !IsBaseCode(code) {
					if _, ok := AccessHostLabel("box", code, AccessKindHTTP); ok {
						t.Fatalf("%q is not a base code", code)
					}
					continue
				}
				bases++
				for _, kind := range kinds {
					label, ok := AccessHostLabel("box", code, kind)
					if !ok {
						t.Fatalf("%s %s", code, kind)
					}
					dev, base, got := ParseAccessHost(label)
					if dev != "box" || base != code || got != kind {
						t.Fatalf("%s %s -> %q parses as %q %q %q", code, kind, label, dev, base, got)
					}
					if kind == AccessKindHTTP && label != WebHostLabel("box", code) {
						t.Fatalf("http label %q", label)
					}
					who := code + "/" + kind
					if other, dup := seen[label]; dup {
						t.Fatalf("%q is both %s and %s", label, other, who)
					}
					seen[label] = who
				}
			}
		}
	}
	if want := 36 * 36 * 6; bases != want {
		t.Fatalf("%d base codes of length 3, want %d", bases, want)
	}
}

func TestParseAccessHostEdges(t *testing.T) {
	if d, c, kind := ParseAccessHost("a--b-ab2"); d != "a--b" || c != "ab0" || kind != AccessKindVNC {
		t.Errorf("%q %q %q", d, c, kind)
	}
	// slots 4 and 5 are reserved
	for _, last := range []string{"4", "5", "a", "b", "g", "h", "m", "n", "s", "t", "y", "z"} {
		if _, _, kind := ParseAccessHost("kali-ab" + last); kind != AccessKindUnknown {
			t.Errorf("last %q: kind %q, want the reserved slot", last, kind)
		}
	}
	// not an access host: no hyphen, a code of the wrong length or alphabet, a bad device name
	for _, label := range []string{"kali", "-abc", "kali-ab", "kali-abcde", "kali-AB0", "kali-a_0", "Kali-ab0", "kali--ab0", "-kali-ab0"} {
		if d, c, kind := ParseAccessHost(label); kind != "" {
			t.Errorf("%q parsed as %q %q %q", label, d, c, kind)
		}
	}
	// a device name with hyphens keeps them
	if d, c, kind := ParseAccessHost("my-box-ab1"); d != "my-box" || c != "ab0" || kind != AccessKindTerminal {
		t.Errorf("%q %q %q", d, c, kind)
	}
	// the published rule for a client: a console host's last character has an index mod 6 of 1
	for _, last := range "0123456789abcdefghijklmnopqrstuvwxyz" {
		_, _, kind := ParseAccessHost("kali-ab" + string(last))
		idx := strings.IndexRune("0123456789abcdefghijklmnopqrstuvwxyz", last)
		if (kind == AccessKindTerminal) != (idx%6 == 1) {
			t.Errorf("last %q: kind %q", last, kind)
		}
	}
}
