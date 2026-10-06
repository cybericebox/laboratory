package profiles

import (
	"slices"
	"testing"
)

func TestAliasesResolveToTheCatalog(t *testing.T) {
	for name, want := range map[string]string{"": Standard, "basic": Standard, "service": Standard, "standard": Standard, "net": Extended, "debug": Extended, "extended": Extended} {
		if got, ok := Resolve(name); !ok || got != want {
			t.Errorf("Resolve(%q) = %q %v, want %q", name, got, ok, want)
		}
	}
	if _, ok := Resolve("root"); ok {
		t.Error("an unknown name must not resolve")
	}
}

// The caps of the doc: standard adds SYS_PTRACE, IPC_LOCK, LINUX_IMMUTABLE; extended adds NET_RAW and NET_ADMIN and /dev/net/tun.
func TestCatalogCaps(t *testing.T) {
	std, ext := Get(Standard), Get(Extended)
	if !slices.Equal(std.Caps, []string{"SYS_PTRACE", "IPC_LOCK", "LINUX_IMMUTABLE"}) || std.TUN {
		t.Errorf("standard = %+v", std)
	}
	for _, c := range []string{"SYS_PTRACE", "IPC_LOCK", "LINUX_IMMUTABLE", "NET_RAW", "NET_ADMIN"} {
		if !slices.Contains(ext.Caps, c) {
			t.Errorf("extended lacks %s", c)
		}
	}
	if !ext.TUN {
		t.Error("extended gets /dev/net/tun")
	}
}

// No profile, and not the base set, ever carries a forbidden capability.
func TestNothingIsEverGrantedFromTheNeverList(t *testing.T) {
	for _, id := range IDs() {
		for _, c := range append(append([]string{}, Base...), Get(id).Caps...) {
			if slices.Contains(Never, c) {
				t.Errorf("%s grants %s", id, c)
			}
		}
	}
}

func TestEnabled(t *testing.T) {
	got, err := Enabled([]string{"standard", "extended", "standard", ""})
	if err != nil || !slices.Equal(got, []string{"standard", "extended"}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := Enabled([]string{"standard", "root"}); err == nil {
		t.Fatal("an unknown ID must be refused")
	}
	if !IsEnabled("net", []string{Extended}) || IsEnabled("basic", []string{Extended}) || IsEnabled("nope", []string{Standard}) {
		t.Error("IsEnabled follows the aliases")
	}
}
