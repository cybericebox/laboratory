//go:build linux

package registrygated

import (
	"testing"

	"github.com/cybericebox/laboratory/pkg/config"
)

// A manifest by tag is refused unless the operator says otherwise: the default is digest-only.
func TestRequireDigestIsOnByDefault(t *testing.T) {
	var cfg Config
	if err := config.Load(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.RequireDigest {
		t.Error("GATE_REQUIRE_DIGEST must default to true")
	}
	t.Setenv("GATE_REQUIRE_DIGEST", "false")
	var off Config
	if err := config.Load(&off); err != nil || off.RequireDigest {
		t.Errorf("GATE_REQUIRE_DIGEST=false must switch it off: %v %v", off.RequireDigest, err)
	}
}
