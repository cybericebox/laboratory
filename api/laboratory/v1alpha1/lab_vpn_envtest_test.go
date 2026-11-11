package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// vpn.reverseConnections (the lab may start connections towards VPN clients) belongs to the VPN segment and needs the VPN.
func TestReverseConnectionsValidation(t *testing.T) {
	dir := os.Getenv("KUBEBUILDER_ASSETS")
	if dir == "" {
		dirs, _ := filepath.Glob("../../../bin/k8s/*")
		if len(dirs) == 0 {
			t.Fatal("no envtest assets: set KUBEBUILDER_ASSETS or run make setup-envtest")
		}
		dir = dirs[len(dirs)-1]
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: dir,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
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
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "g"}}); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, vpn, internet LabNetworkSpec) *Lab {
		return &Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g"}, Spec: LabSpec{VPN: vpn, Internet: internet}}
	}
	for name, tc := range map[string]struct {
		lab  *Lab
		want string // empty: accepted
	}{
		"default":                    {mk("a", LabNetworkSpec{Enabled: true}, LabNetworkSpec{}), ""},
		"on with a vpn":              {mk("b", LabNetworkSpec{Enabled: true, ReverseConnections: true}, LabNetworkSpec{}), ""},
		"off without a vpn":          {mk("c", LabNetworkSpec{}, LabNetworkSpec{}), ""},
		"on without a vpn":           {mk("d", LabNetworkSpec{ReverseConnections: true}, LabNetworkSpec{}), "vpn.reverseConnections needs vpn.enabled"},
		"on on the internet segment": {mk("e", LabNetworkSpec{Enabled: true}, LabNetworkSpec{Enabled: true, ReverseConnections: true}), "belongs to the vpn segment only"},
	} {
		err := c.Create(ctx, tc.lab)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: rejected: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
	}
}
