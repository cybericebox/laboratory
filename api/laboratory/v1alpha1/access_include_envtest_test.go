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

// The granular access rule as the CRD itself enforces it: an include is a subnet of the gateway's /24 (prefix 24..32, host the first address of
// the subnet), only an allow rule that names gateways has one.
func TestAccessIncludeValidation(t *testing.T) {
	dir := os.Getenv("KUBEBUILDER_ASSETS")
	if dir == "" {
		dirs, _ := filepath.Glob("../../../bin/k8s/*")
		if len(dirs) == 0 {
			t.Fatal("no envtest assets: set KUBEBUILDER_ASSETS or run make setup-envtest")
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
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "g"}}); err != nil {
		t.Fatal(err)
	}
	allow := func(gateways []string, in ...AccessInclude) LabGroupAccessRule {
		return LabGroupAccessRule{Action: LabGroupAccessAllow, LabNames: []string{"web"}, GatewayNames: gateways, Include: in}
	}
	n := 0
	for name, tc := range map[string]struct {
		rule LabGroupAccessRule
		want string // empty: accepted
	}{
		"a connector, whole /24":        {allow([]string{"office"}), ""},
		"one device":                    {allow([]string{"office"}, AccessInclude{Host: 10, Prefix: 32}), ""},
		"several subnets":               {allow([]string{"office"}, AccessInclude{Host: 10, Prefix: 32}, AccessInclude{Host: 64, Prefix: 26}, AccessInclude{Host: 0, Prefix: 24}), ""},
		"allow without gateways":        {allow(nil), ""},
		"allow without labs":            {LabGroupAccessRule{Action: LabGroupAccessAllow, GatewayNames: []string{"office"}}, "an allow rule names the laboratories"},
		"prefix wider than the /24":     {allow([]string{"office"}, AccessInclude{Host: 0, Prefix: 16}), "should be greater than or equal to 24"},
		"prefix longer than 32":         {allow([]string{"office"}, AccessInclude{Host: 0, Prefix: 33}), "should be less than or equal to 32"},
		"host outside the segment":      {allow([]string{"office"}, AccessInclude{Host: 256, Prefix: 32}), "should be less than or equal to 255"},
		"host not at the subnet start":  {allow([]string{"office"}, AccessInclude{Host: 5, Prefix: 29}), "first address"},
		"include without gateways":      {allow(nil, AccessInclude{Host: 10, Prefix: 32}), "include needs an allow rule that names gateways"},
		"include on a deny rule":        {LabGroupAccessRule{Action: LabGroupAccessDeny, GatewayNames: []string{"office"}, Include: []AccessInclude{{Host: 10, Prefix: 32}}}, "include needs an allow rule that names gateways"},
		"deny of every lab and gateway": {LabGroupAccessRule{Action: LabGroupAccessDeny}, ""},
	} {
		n++
		p := &LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p" + string(rune('a'+n)), Namespace: "g"}, Spec: LabGroupAccessPolicySpec{Rules: []LabGroupAccessRule{tc.rule}}}
		err := c.Create(ctx, p)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: rejected: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
	}
}
