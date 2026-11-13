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

// The gateways a lab declares: the shape the CRD itself enforces (names, kinds, one of network and gateway in a reference). The number
// of them is the chart's limit and is checked in code (internal/gatewayspec).
func TestGatewayDeclarationValidation(t *testing.T) {
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
	withGateways := func(name string, g ...GatewayTemplate) *Lab {
		return &Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g"}, Spec: LabSpec{Gateways: g}}
	}
	withRef := func(name string, ref NetworkIPRef) *Lab {
		return &Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g"}, Spec: LabSpec{Devices: []DeviceTemplate{{
			Name: "web", Type: DeviceTypeContainer,
			Interfaces: []InterfaceSpec{{Name: "eth0", Addr: &AddrSpec{Type: AddrTypeStatic, AddressRef: &ref}}},
		}}}}
	}
	for name, tc := range map[string]struct {
		lab  *Lab
		want string // empty: accepted
	}{
		"a vpn and an internet gateway": {withGateways("a", GatewayTemplate{Name: "attack", Kind: GatewayKindVPN}, GatewayTemplate{Name: "out", Kind: GatewayKindInternet}), ""},
		"reverse connections on vpn":    {withGateways("b", GatewayTemplate{Name: "attack", Kind: GatewayKindVPN, ReverseConnections: true}), ""},
		"reverse connections on inet":   {withGateways("c", GatewayTemplate{Name: "out", Kind: GatewayKindInternet, ReverseConnections: true}), "reverseConnections belongs to a vpn gateway only"},
		"upper case":                    {withGateways("f", GatewayTemplate{Name: "Attack", Kind: GatewayKindVPN}), "Invalid value"},
		"unknown kind":                  {withGateways("g", GatewayTemplate{Name: "attack", Kind: "tunnel"}), "Unsupported value"},
		"same name twice":               {withGateways("h", GatewayTemplate{Name: "a", Kind: GatewayKindVPN}, GatewayTemplate{Name: "a", Kind: GatewayKindInternet}), "Duplicate value"},
		"a name of 36 characters":       {withGateways("i", GatewayTemplate{Name: strings.Repeat("a", 36), Kind: GatewayKindVPN}), "Too long"},
		"a gateway may be named vpn":    {withGateways("n", GatewayTemplate{Name: "vpn", Kind: GatewayKindVPN}), ""},
		"reference by gateway":          {withRef("k", NetworkIPRef{Gateway: "attack", Host: 10}), ""},
		"reference without a gateway":   {withRef("m", NetworkIPRef{Host: 10}), "should be at least 1 chars"},
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
