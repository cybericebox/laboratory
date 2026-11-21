package imagepolicy

import (
	"errors"
	"testing"
)

// A platform reference is refused, whatever it looks like, and is never read as a Docker Hub image.
func TestPlatformReferencesAreRefused(t *testing.T) {
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for _, ref := range []string{"platform:tools@" + digest, "platform:tools", "platform:tools:1", "platform:"} {
		if err := (Policy{}).Check(ref); !errors.Is(err, ErrNotPlatform) {
			t.Errorf("%q: %v", ref, err)
		}
	}
	// the node form of the old platform registry is just a reference to the cache, which a tenant may not name
	if err := (Policy{CachePrefixes: []string{"localhost:5035"}}).Check("localhost:5035/platform/tools@" + digest); err == nil {
		t.Error("the old node form must stay refused")
	}
}
