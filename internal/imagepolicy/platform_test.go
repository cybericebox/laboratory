package imagepolicy

import (
	"errors"
	"strings"
	"testing"
)

const (
	digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	prefix = "localhost:5035"
)

func TestParsePlatform(t *testing.T) {
	for ref, want := range map[string]bool{
		"platform:tools@" + digest:                           true,
		"platform:a.b-c_d@" + digest:                         true,
		"platform:" + strings.Repeat("a", 64) + "@" + digest: true,
		"platform:" + strings.Repeat("a", 65) + "@" + digest: false,
		"platform:tools":                                     false,
		"platform:tools:1":                                   false,
		"platform:tools@sha256:00":                           false,
		"platform:Tools@" + digest:                           false,
		"platform:a/b@" + digest:                             false,
		"platform:-a@" + digest:                              false,
		"platform:a--b@" + digest:                            false,
		"platform:@" + digest:                                false,
		"platform:tools@sha256:" + strings.ToUpper(strings.Repeat("a", 64)): false,
		"nginx":                     false,
		"platformx:tools@" + digest: false,
	} {
		if _, ok := ParsePlatform(ref); ok != want {
			t.Errorf("ParsePlatform(%q) = %v, want %v", ref, ok, want)
		}
	}
}

func TestParseNodeIsExactlyThePlatformNamespaceByDigest(t *testing.T) {
	for ref, want := range map[string]bool{
		prefix + "/platform/tools@" + digest:      true,
		prefix + "/platform/tools:1":              false,
		prefix + "/platform/a/b@" + digest:        false,
		prefix + "/platformx/tools@" + digest:     false,
		prefix + "/lab/ns/l/d@" + digest:          false,
		prefix + "/base@" + digest:                false,
		prefix + "/docker.io/library/x@" + digest: false,
		"localhost:5036/platform/tools@" + digest: false,
		"platform/tools@" + digest:                false,
	} {
		if _, ok := ParseNode(ref, prefix); ok != want {
			t.Errorf("ParseNode(%q) = %v, want %v", ref, ok, want)
		}
	}
	if _, ok := ParseNode(prefix+"/platform/tools@"+digest, ""); ok {
		t.Error("no prefix, no node form")
	}
}

func TestMapOnlyChangesPlatformReferences(t *testing.T) {
	p := Policy{PlatformPrefix: prefix}
	if got := p.Map("platform:tools@" + digest); got != prefix+"/platform/tools@"+digest {
		t.Errorf("Map = %q", got)
	}
	for _, same := range []string{"nginx", "ghcr.io/a/b:1", prefix + "/platform/tools@" + digest, "platform:bad"} {
		if got := p.Map(same); got != same {
			t.Errorf("Map(%q) = %q", same, got)
		}
	}
	if got := (Policy{}).Map("platform:tools@" + digest); got != "platform:tools@"+digest {
		t.Errorf("with no prefix a reference stays as it is: %q", got)
	}
}

func TestCheckPlatformOnly(t *testing.T) {
	p := Policy{PlatformOnly: true, PlatformPrefix: prefix, CachePrefixes: []string{prefix}, Deny: []string{"ghcr.io/x/"}}
	for _, ok := range []string{"platform:tools@" + digest, prefix + "/platform/tools@" + digest} {
		if err := p.Check(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"nginx", "docker.io/library/nginx@" + digest, "ghcr.io/a/b:1", prefix + "/docker.io/library/nginx:1",
		prefix + "/lab/ns/l/d@" + digest, prefix + "/base@" + digest, prefix + "/platform/tools:1", prefix + "/platform/a/b@" + digest,
		"localhost:5000/platform/tools@" + digest, "platform:tools", "platform:tools:1", "",
	} {
		err := p.Check(bad)
		if !errors.Is(err, ErrNotPlatform) {
			t.Errorf("%q: want ErrNotPlatform, got %v", bad, err)
		}
	}
}

func TestCheckWithoutPlatformOnly(t *testing.T) {
	p := Policy{PlatformPrefix: prefix, CachePrefixes: []string{prefix}, Allow: []string{"ghcr.io/acme/"}}
	// the platform's own image passes the tenant's allow list: the platform chose it
	if err := p.Check("platform:tools@" + digest); err != nil {
		t.Errorf("platform image: %v", err)
	}
	// a reference that merely starts like one is refused, never read as a Docker Hub image
	if err := p.Check("platform:tools:1"); !errors.Is(err, ErrNotPlatform) {
		t.Errorf("platform:tools:1: %v", err)
	}
	// the node form is still the cache refusal, not the platform one
	err := p.Check(prefix + "/platform/tools@" + digest)
	if err == nil || errors.Is(err, ErrNotPlatform) {
		t.Errorf("node form without platform-only: %v", err)
	}
	if err := p.Check("ghcr.io/acme/app:1"); err != nil {
		t.Errorf("allowed image: %v", err)
	}
	if err := p.Check("docker.io/library/nginx"); err == nil || errors.Is(err, ErrNotPlatform) {
		t.Errorf("not on the allow list: %v", err)
	}
}

func TestIsPlatform(t *testing.T) {
	p := Policy{PlatformPrefix: prefix}
	for ref, want := range map[string]bool{
		"platform:tools@" + digest: true, prefix + "/platform/tools@" + digest: true, "nginx": false, prefix + "/lab/x/y/z@" + digest: false,
	} {
		if got := p.IsPlatform(ref); got != want {
			t.Errorf("IsPlatform(%q) = %v", ref, got)
		}
	}
}
