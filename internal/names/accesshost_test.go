package names

import (
	"strings"
	"testing"
)

func TestAccessHostLabels(t *testing.T) {
	for _, c := range []struct{ code, kind, want string }{
		{"ab0", AccessKindHTTP, "kali-ab0w"},
		{"ab0", AccessKindTerminal, "kali-ab0t"},
		{"ab0", AccessKindVNC, "kali-ab0v"},
		{"ab0", AccessKindRDP, "kali-ab0r"},
		{"x7k2", AccessKindTerminal, "kali-x7k2t"},
	} {
		got, ok := AccessHostLabel("kali", c.code, c.kind)
		if !ok || got != c.want {
			t.Errorf("%s %s: %q %v, want %q", c.code, c.kind, got, ok, c.want)
		}
	}
	if l, _ := AccessHostLabel("kali", "x7c", AccessKindHTTP); l != WebHostLabel("kali", "x7c") || l != "kali-x7cw" {
		t.Errorf("http label %q differs from the web host label", l)
	}
	if got := WorkloadName("kali", "x7c", "legacy"); got != "kali-x7c" {
		t.Errorf("workload name %q has no kind character", got)
	}
	for _, bad := range []struct{ code, kind string }{{"x7c", "ssh"}, {"", AccessKindHTTP}, {"X7c", AccessKindHTTP}, {"x-c", AccessKindHTTP}} {
		if _, ok := AccessHostLabel("kali", bad.code, bad.kind); ok {
			t.Errorf("%+v has a label", bad)
		}
	}
	long := strings.Repeat("a", MaxDeviceNameLen)
	for _, kind := range []string{AccessKindHTTP, AccessKindTerminal, AccessKindVNC, AccessKindRDP} {
		if l, _ := AccessHostLabel(long, strings.Repeat("z", WebCodeMaxLen), kind); len(l) != 41 {
			t.Fatalf("%s label is %d characters", kind, len(l))
		}
	}
}

// Over every code of length 3 and every kind: the label parses back to the device, the code and the kind; no two (code, kind) give
// the same label; and the web host of a device never reads as the console of any device.
func TestAccessHostsExhaustive(t *testing.T) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	kinds := []string{AccessKindHTTP, AccessKindTerminal, AccessKindVNC, AccessKindRDP}
	seen := map[string]string{}
	for i := 0; i < len(alphabet); i++ {
		for j := 0; j < len(alphabet); j++ {
			for k := 0; k < len(alphabet); k++ {
				code := string([]byte{alphabet[i], alphabet[j], alphabet[k]})
				for _, kind := range kinds {
					label, ok := AccessHostLabel("box", code, kind)
					if !ok {
						t.Fatalf("%s %s", code, kind)
					}
					dev, gotCode, got := ParseAccessHost(label)
					if dev != "box" || gotCode != code || got != kind {
						t.Fatalf("%s %s -> %q parses as %q %q %q", code, kind, label, dev, gotCode, got)
					}
					if kind == AccessKindHTTP && (label != WebHostLabel("box", code) || got == AccessKindTerminal) {
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
	if want := 36 * 36 * 36 * len(kinds); len(seen) != want {
		t.Fatalf("%d labels, want %d", len(seen), want)
	}
}

func TestParseAccessHostEdges(t *testing.T) {
	// the kind is the last character; every other value stands for nothing yet
	for _, last := range "0123456789abcdefghijklmnopqrstuvwxyz" {
		_, _, kind := ParseAccessHost("kali-ab0" + string(last))
		want := map[rune]string{'w': AccessKindHTTP, 't': AccessKindTerminal, 'v': AccessKindVNC, 'r': AccessKindRDP}[last]
		if want == "" {
			want = AccessKindUnknown
		}
		if kind != want {
			t.Errorf("last %q: kind %q, want %q", last, kind, want)
		}
	}
	// the length of the code does not matter
	for _, label := range []string{"kali-at", "kali-abt", "kali-abct", "kali-abcdt", "kali-abcdeft"} {
		if d, c, kind := ParseAccessHost(label); d != "kali" || c != strings.TrimSuffix(strings.TrimPrefix(label, "kali-"), "t") || kind != AccessKindTerminal {
			t.Errorf("%q: %q %q %q", label, d, c, kind)
		}
	}
	// a device name with hyphens keeps them
	if d, c, kind := ParseAccessHost("my--box-ab1t"); d != "my--box" || c != "ab1" || kind != AccessKindTerminal {
		t.Errorf("%q %q %q", d, c, kind)
	}
	// not an access host: no hyphen, no code, a code of the wrong alphabet, a bad device name
	for _, label := range []string{"kali", "-abt", "kali-t", "kali-", "kali-AB0t", "kali-a_0t", "Kali-ab0t", "kali--ab0t", "-kali-ab0t", strings.Repeat("d", 36) + "-ab0t"} {
		if d, c, kind := ParseAccessHost(label); kind != "" {
			t.Errorf("%q parsed as %q %q %q", label, d, c, kind)
		}
	}
}
