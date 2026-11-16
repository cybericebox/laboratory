package registrygate

import "testing"

func TestDiskWatch(t *testing.T) {
	if (&DiskWatch{Path: t.TempDir(), HighWater: 0}).Full() {
		t.Fatal("no mark, never full")
	}
	// a volume that can be measured: 101 percent is never reached, and the answer is kept for the TTL
	d := &DiskWatch{Path: t.TempDir(), HighWater: 101}
	if d.Full() {
		t.Fatal("a mark above 100% is never reached")
	}
	if (&DiskWatch{Path: "/no/such/path", HighWater: 1}).Full() {
		t.Fatal("a volume that cannot be measured is not full")
	}
}

func TestParseAnonymousFill(t *testing.T) {
	regs, err := ParseRegistries([]byte(`[{"name":"docker.io","url":"https://registry-1.docker.io"},{"name":"ghcr.io","url":"https://ghcr.io"}]`))
	if err != nil {
		t.Fatal(err)
	}
	fill, err := ParseAnonymousFill([]byte(`{"auths":{"https://index.docker.io/v1/":{"username":"u","password":"p"},"ghcr.io":{"auth":"Zzpx"},"other.io":{"username":"x","password":"y"}}}`), regs)
	if err != nil {
		t.Fatal(err)
	}
	if fill["docker.io"] != (Creds{"u", "p"}) || fill["ghcr.io"] != (Creds{"g", "q"}) || len(fill) != 2 {
		t.Fatalf("%+v", fill)
	}
	if _, err := ParseAnonymousFill([]byte(`nope`), regs); err == nil {
		t.Fatal("garbage must be refused")
	}
}

func TestFillCredsAreUsedOnlyForAnonymousPulls(t *testing.T) {
	c := &Cache{Fill: AnonymousFill{"docker.io": {"u", "p"}}}
	if cr, _ := c.fillCreds(caller{}, "docker.io"); cr != (Creds{"u", "p"}) {
		t.Fatal("an anonymous pull is filled with the registry's login")
	}
	if cr, _ := c.fillCreds(caller{}, "ghcr.io"); cr != (Creds{}) {
		t.Fatal("no login for another registry")
	}
	if cr, k := c.fillCreds(caller{creds: Creds{"a", "b"}, hash: "h"}, "docker.io"); cr != (Creds{"a", "b"}) || k != "h" {
		t.Fatal("a pull with credentials is filled with its own, never with the login")
	}
}
