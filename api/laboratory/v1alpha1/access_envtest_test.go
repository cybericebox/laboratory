package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	corev1 "k8s.io/api/core/v1"
)

// The access list of a device is checked by the API server itself: kinds, one entry per kind, and the fields each kind takes.
func TestDeviceAccessValidation(t *testing.T) {
	dir := os.Getenv("KUBEBUILDER_ASSETS")
	if dir == "" {
		dirs, _ := filepath.Glob("../../../bin/k8s/*")
		if len(dirs) == 0 {
			dirs, _ = filepath.Glob("../../../../../bin/k8s/*")
		}
		if len(dirs) == 0 {
			t.Skip("no envtest assets")
		}
		dir = dirs[len(dirs)-1]
	}
	env := &envtest.Environment{BinaryAssetsDirectory: dir, CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	if err := AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}); err != nil {
		t.Fatal(err)
	}
	n := 0
	try := func(access ...DeviceAccess) error {
		n++
		lab := &Lab{ObjectMeta: metav1.ObjectMeta{Name: "l" + strings.Repeat("x", n), Namespace: "ns"}}
		lab.Spec.Devices = []DeviceTemplate{{Name: "web", Type: DeviceTypeContainer, Access: access}}
		return c.Create(ctx, lab)
	}
	for name, c := range map[string]struct {
		access []DeviceAccess
		want   string
	}{
		"http":                 {[]DeviceAccess{{Kind: AccessHTTP, Port: 8080}}, ""},
		"https":                {[]DeviceAccess{{Kind: AccessHTTP, Port: 443, Protocol: "https"}}, ""},
		"terminal":             {[]DeviceAccess{{Kind: AccessTerminal, Command: []string{"/bin/bash"}, User: "ctf"}}, ""},
		"all four":             {[]DeviceAccess{{Kind: AccessHTTP, Port: 80}, {Kind: AccessTerminal}, {Kind: AccessVNC, Port: 5900}, {Kind: AccessRDP, Port: 3389}}, ""},
		"two http":             {[]DeviceAccess{{Kind: AccessHTTP, Port: 80}, {Kind: AccessHTTP, Port: 81}}, "Duplicate value"},
		"unknown kind":         {[]DeviceAccess{{Kind: "ssh", Port: 22}}, "Unsupported value"},
		"http without a port":  {[]DeviceAccess{{Kind: AccessHTTP}}, "need a port"},
		"port out of range":    {[]DeviceAccess{{Kind: AccessVNC, Port: 70000}}, "70000"},
		"terminal with a port": {[]DeviceAccess{{Kind: AccessTerminal, Port: 22}}, "takes none"},
		"vnc with a protocol":  {[]DeviceAccess{{Kind: AccessVNC, Port: 5900, Protocol: "http"}}, "only an http access has a protocol"},
		"rdp with a command":   {[]DeviceAccess{{Kind: AccessRDP, Port: 3389, Command: []string{"x"}}}, "only a terminal access"},
		"http with a user":     {[]DeviceAccess{{Kind: AccessHTTP, Port: 80, User: "root"}}, "only a terminal access"},
		"protocol not http(s)": {[]DeviceAccess{{Kind: AccessHTTP, Port: 80, Protocol: "ftp"}}, "Unsupported value"},
	} {
		t.Run(name, func(t *testing.T) {
			err := try(c.access...)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("must be accepted: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}
