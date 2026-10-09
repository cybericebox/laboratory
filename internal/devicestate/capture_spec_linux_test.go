//go:build linux

package devicestate

import (
	"context"
	"errors"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Fault injection is at the OCI-spec read used by ContainerdRuntime.Inspect.
func TestRequiredCaptureOCISpecErrorNeverBecomesIdentity(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	c, err := r.rt.Inspect(context.Background(), r.pod.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	inspectContainerSpec(context.Background(), &c, func(context.Context) (*specs.Spec, error) { return nil, errors.New("OCI spec unavailable") }, t.TempDir())
	if c.OwnershipKnown {
		t.Fatal("OCI read error was marked as known no-userns")
	}
	r.e.Runtime = &metadataRuntime{fakeRuntime: r.rt, known: c.OwnershipKnown, ids: c.IDs}
	r.rt.setDiff(tarOf(map[string]string{"work": "changed"}))
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err == nil || result.Result == "Succeeded" {
		t.Fatalf("spec error permitted capture: %+v %v", result, err)
	}
}

func TestRequiredCaptureOCIMappingsDistinguishKnownAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  *specs.Spec
		known bool
	}{
		{"no-userns", &specs.Spec{Linux: &specs.Linux{}}, true},
		{"missing-linux", &specs.Spec{}, false},
		{"userns-no-maps", &specs.Spec{Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{{Type: specs.UserNamespace}}}}, false},
		{"mapped", &specs.Spec{Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{{Type: specs.UserNamespace}}, UIDMappings: []specs.LinuxIDMapping{{ContainerID: 0, HostID: 100000, Size: 65536}}, GIDMappings: []specs.LinuxIDMapping{{ContainerID: 0, HostID: 200000, Size: 65536}}}}, true},
		{"only-uid", &specs.Spec{Linux: &specs.Linux{UIDMappings: []specs.LinuxIDMapping{{Size: 65536}}}}, false},
		{"zero-size", &specs.Spec{Linux: &specs.Linux{UIDMappings: []specs.LinuxIDMapping{{Size: 0}}, GIDMappings: []specs.LinuxIDMapping{{Size: 0}}}}, false},
		{"overflow", &specs.Spec{Linux: &specs.Linux{UIDMappings: []specs.LinuxIDMapping{{HostID: 4294967295, Size: 2}}, GIDMappings: []specs.LinuxIDMapping{{Size: 65536}}}}, false},
		{"overlap", &specs.Spec{Linux: &specs.Linux{UIDMappings: []specs.LinuxIDMapping{{Size: 100}, {ContainerID: 50, HostID: 200, Size: 100}}, GIDMappings: []specs.LinuxIDMapping{{Size: 65536}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Container{}
			inspectContainerSpec(context.Background(), &c, func(context.Context) (*specs.Spec, error) { return tc.spec, nil }, t.TempDir())
			if c.OwnershipKnown != tc.known {
				t.Fatalf("ownership known=%v want=%v", c.OwnershipKnown, tc.known)
			}
			if tc.name == "mapped" {
				u, ok := c.IDs.UID.ToContainer(100123)
				g, gok := c.IDs.GID.ToContainer(200456)
				if !ok || !gok || u != 123 || g != 456 {
					t.Fatalf("bad OCI mapping: uid=%d gid=%d", u, g)
				}
			}
		})
	}
}
