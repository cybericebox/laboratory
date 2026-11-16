package chart_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

const (
	vpnLabel      = "laboratory.cybericebox.com/vpn-enabled"
	internetLabel = "laboratory.cybericebox.com/internet-enabled"
)

// featureCluster is an API server with the laboratory's CRDs and the admission policies of a chart rendered with the given values.
type featureCluster struct {
	admin    client.Client
	operator client.Client
	other    client.Client
}

func startFeatureCluster(t *testing.T, applyBefore func(admin client.Client), set ...string) *featureCluster {
	t.Helper()
	out, err := helmTemplate(t, append(append([]string{}, set...), "-s", "templates/operator/feature-policy.yaml", "-s", "templates/operator/lab-namespace-policy.yaml")...)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: envtestDir(), CRDDirectoryPaths: []string{"../../charts/laboratory/crds"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start admission API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	mk := func(user string) client.Client {
		c := cfg
		if user != "" {
			c = rest.CopyConfig(cfg)
			c.Impersonate = rest.ImpersonationConfig{UserName: user}
		}
		cl, err := client.New(c, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}
	f := &featureCluster{admin: mk(""), operator: mk(operatorUser), other: mk(somebodyElse)}
	ctx := context.Background()
	for _, u := range []string{operatorUser, somebodyElse} {
		name := "t-" + strings.NewReplacer(":", "-", "_", "-").Replace(u)
		if err := f.admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: u}}}); err != nil {
			t.Fatal(err)
		}
	}
	if applyBefore != nil {
		applyBefore(f.admin)
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		var obj unstructured.Unstructured
		if yaml.Unmarshal([]byte(doc), &obj.Object) != nil || obj.Object == nil {
			continue
		}
		if err := f.admin.Create(ctx, &obj); err != nil {
			t.Fatal(err)
		}
	}
	// wait until the policies are enforced: a probe namespace that carries a switch label from somebody else is refused
	deadline := time.Now().Add(40 * time.Second)
	for i := 0; ; i++ {
		probe := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "probe-", Labels: map[string]string{vpnLabel: "false"}}}
		if err := f.other.Create(ctx, probe); apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the policies are not enforced")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return f
}

func (f *featureCluster) group(t *testing.T, name string, vpn, internet *string) string {
	t.Helper()
	ns := "lg-" + name
	labels := map[string]string{groupLabel: name}
	if vpn != nil {
		labels[vpnLabel] = *vpn
	}
	if internet != nil {
		labels[internetLabel] = *internet
	}
	if err := f.operator.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: labels}}); err != nil {
		t.Fatal(err)
	}
	return ns
}

func str(s string) *string { return &s }

func lab(ns, name string, mod func(*laboratoryv1alpha1.LabSpec)) *laboratoryv1alpha1.Lab {
	l := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if mod != nil {
		mod(&l.Spec)
	}
	return l
}

func withGateway(kind laboratoryv1alpha1.GatewayKind) func(*laboratoryv1alpha1.LabSpec) {
	return func(s *laboratoryv1alpha1.LabSpec) {
		s.Gateways = append(s.Gateways, laboratoryv1alpha1.GatewayTemplate{Name: "gw-" + string(kind), Kind: kind})
	}
}

func withAccess(kind laboratoryv1alpha1.AccessKind) func(*laboratoryv1alpha1.LabSpec) {
	return func(s *laboratoryv1alpha1.LabSpec) {
		a := laboratoryv1alpha1.DeviceAccess{Kind: kind}
		if kind != laboratoryv1alpha1.AccessTerminal {
			a.Port = 8080
		}
		s.Devices = append(s.Devices, laboratoryv1alpha1.DeviceTemplate{Name: "dev", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx", Access: []laboratoryv1alpha1.DeviceAccess{a}})
	}
}

func refused(t *testing.T, err error, reason string) {
	t.Helper()
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("must be refused with %q: %v", reason, err)
	}
}

func accepted(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("must be accepted: %v", err)
	}
}

// With every cluster switch OFF the API server refuses what needs the feature, with a message that names it, and still lets an object made
// before the switch was turned off be updated (a finalizer, a label) and deleted.
func TestFeatureAdmissionClusterSwitchesOff(t *testing.T) {
	ctx := context.Background()
	var old *laboratoryv1alpha1.Lab
	f := startFeatureCluster(t, func(admin client.Client) {
		// a lab made while the VPN was on, before the policies exist
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lg-old"}}); err != nil {
			t.Fatal(err)
		}
		old = lab("lg-old", "made-before", withGateway(laboratoryv1alpha1.GatewayKindVPN))
		if err := admin.Create(ctx, old); err != nil {
			t.Fatal(err)
		}
	}, "--set", "vpn.enabled=false", "--set", "inetGateway.enabled=false", "--set", "proxy.enabled=false")
	ns := f.group(t, "off", nil, nil)

	refused(t, f.other.Create(ctx, lab(ns, "vpn", withGateway(laboratoryv1alpha1.GatewayKindVPN))), "VPN_UNAVAILABLE")
	refused(t, f.other.Create(ctx, lab(ns, "net", withGateway(laboratoryv1alpha1.GatewayKindInternet))), "INTERNET_UNAVAILABLE")
	refused(t, f.other.Create(ctx, lab(ns, "web", withAccess(laboratoryv1alpha1.AccessHTTP))), "ACCESS_UNAVAILABLE")
	refused(t, f.other.Create(ctx, lab(ns, "chk", func(s *laboratoryv1alpha1.LabSpec) { s.Checks = &laboratoryv1alpha1.LabChecksSpec{} })), "CHECKS_UNAVAILABLE")
	accepted(t, f.other.Create(ctx, lab(ns, "plain", nil)))

	// a client has no VPN to connect to; a policy has neither the VPN nor the L7 proxy to read it
	refused(t, f.other.Create(ctx, &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: ns}, Spec: laboratoryv1alpha1.LabGroupClientSpec{PublicKey: "k"}}), "VPN_UNAVAILABLE")
	refused(t, f.other.Create(ctx, &laboratoryv1alpha1.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns}}), "VPN_UNAVAILABLE")

	// a group cannot turn a server on that the laboratory does not run; off, or unset, is fine
	grp := func(name string, vpn, internet *bool) *laboratoryv1alpha1.LabGroup {
		return &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: laboratoryv1alpha1.LabGroupSpec{
			VPN: laboratoryv1alpha1.LabGroupVPNSpec{Enabled: vpn}, Gateway: laboratoryv1alpha1.LabGroupGatewaySpec{Enabled: internet}}}
	}
	on, off := true, false
	refused(t, f.other.Create(ctx, grp("g-vpn", &on, nil)), "VPN_UNAVAILABLE")
	refused(t, f.other.Create(ctx, grp("g-net", nil, &on)), "INTERNET_UNAVAILABLE")
	accepted(t, f.other.Create(ctx, grp("g-off", &off, &off)))
	accepted(t, f.other.Create(ctx, grp("g-none", nil, nil)))
	var cur laboratoryv1alpha1.LabGroup
	accepted(t, f.admin.Get(ctx, client.ObjectKey{Name: "g-off"}, &cur))
	cur.Spec.VPN.Enabled = &on
	refused(t, f.other.Update(ctx, &cur), "VPN_UNAVAILABLE")

	// what was made before the switch went off: metadata updates and deletion still work, a change of the spec does not
	var before laboratoryv1alpha1.Lab
	accepted(t, f.admin.Get(ctx, client.ObjectKeyFromObject(old), &before))
	before.Labels = map[string]string{"touched": "yes"}
	before.Finalizers = []string{"x/y"}
	accepted(t, f.other.Update(ctx, &before))
	before.Spec.Gateways = append(before.Spec.Gateways, laboratoryv1alpha1.GatewayTemplate{Name: "another", Kind: laboratoryv1alpha1.GatewayKindVPN})
	refused(t, f.other.Update(ctx, &before), "VPN_UNAVAILABLE")
	accepted(t, f.admin.Get(ctx, client.ObjectKeyFromObject(old), &before))
	before.Finalizers = nil
	accepted(t, f.other.Update(ctx, &before))
	accepted(t, f.other.Delete(ctx, &before))
}

// With the cluster running everything the switches of a GROUP decide, through the labels the operator puts on its namespace; only the
// operator can set them; a namespace without them is not checked.
func TestFeatureAdmissionGroupSwitches(t *testing.T) {
	ctx := context.Background()
	f := startFeatureCluster(t, nil, "--set", "proxy.rdp.enabled=true", "--set", "proxy.vnc.enabled=true", "--set", "terminal.enabled=true", "--set", "checks.enabled=true")
	vpnOnly := f.group(t, "vpnonly", str("true"), str("false"))
	netOnly := f.group(t, "netonly", str("false"), str("true"))
	legacy := f.group(t, "legacy", nil, nil)

	accepted(t, f.other.Create(ctx, lab(vpnOnly, "vpn", withGateway(laboratoryv1alpha1.GatewayKindVPN))))
	refused(t, f.other.Create(ctx, lab(vpnOnly, "net", withGateway(laboratoryv1alpha1.GatewayKindInternet))), "GROUP_INTERNET_OFF")
	refused(t, f.other.Create(ctx, lab(netOnly, "vpn", withGateway(laboratoryv1alpha1.GatewayKindVPN))), "GROUP_VPN_OFF")
	accepted(t, f.other.Create(ctx, lab(netOnly, "net", withGateway(laboratoryv1alpha1.GatewayKindInternet))))
	accepted(t, f.other.Create(ctx, lab(netOnly, "plain", nil)))
	accepted(t, f.other.Create(ctx, lab(legacy, "both", func(s *laboratoryv1alpha1.LabSpec) {
		withGateway(laboratoryv1alpha1.GatewayKindVPN)(s)
		withGateway(laboratoryv1alpha1.GatewayKindInternet)(s)
	})))
	// every access is served by this cluster
	for _, k := range []laboratoryv1alpha1.AccessKind{laboratoryv1alpha1.AccessHTTP, laboratoryv1alpha1.AccessTerminal, laboratoryv1alpha1.AccessRDP, laboratoryv1alpha1.AccessVNC} {
		accepted(t, f.other.Create(ctx, lab(legacy, "acc-"+string(k), withAccess(k))))
	}

	// clients: only in a group whose VPN is on (or that has no label)
	mkClient := func(ns string) *laboratoryv1alpha1.LabGroupClient {
		return &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: ns}, Spec: laboratoryv1alpha1.LabGroupClientSpec{PublicKey: "k"}}
	}
	accepted(t, f.other.Create(ctx, mkClient(vpnOnly)))
	refused(t, f.other.Create(ctx, mkClient(netOnly)), "GROUP_VPN_OFF")
	accepted(t, f.other.Create(ctx, mkClient(legacy)))
	// the policy is not tied to the group's VPN (the L7 proxy reads it too)
	accepted(t, f.other.Create(ctx, &laboratoryv1alpha1.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: netOnly}}))

	// the labels: the operator sets and changes them, nobody else may
	var n corev1.Namespace
	accepted(t, f.admin.Get(ctx, client.ObjectKey{Name: netOnly}, &n))
	n.Labels[vpnLabel] = "true"
	refused(t, f.other.Update(ctx, &n), "only the operator")
	accepted(t, f.operator.Update(ctx, &n))
	accepted(t, f.other.Create(ctx, lab(netOnly, "vpn-now", withGateway(laboratoryv1alpha1.GatewayKindVPN))))
	refused(t, f.other.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mine", Labels: map[string]string{internetLabel: "true"}}}), "only the operator")
	accepted(t, f.other.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mine-plain", Labels: map[string]string{"app": "x"}}}))
}

// With the L7 proxy and the terminal off (the rest on): every access is refused, and a policy is still read by the VPN pod.
func TestFeatureAdmissionProxyOff(t *testing.T) {
	ctx := context.Background()
	f := startFeatureCluster(t, nil, "--set", "proxy.enabled=false")
	ns := f.group(t, "noproxy", nil, nil)
	for _, k := range []laboratoryv1alpha1.AccessKind{laboratoryv1alpha1.AccessHTTP, laboratoryv1alpha1.AccessTerminal, laboratoryv1alpha1.AccessRDP, laboratoryv1alpha1.AccessVNC} {
		refused(t, f.other.Create(ctx, lab(ns, "acc-"+string(k), withAccess(k))), "ACCESS_UNAVAILABLE")
	}
	accepted(t, f.other.Create(ctx, lab(ns, "vpn", withGateway(laboratoryv1alpha1.GatewayKindVPN))))
	accepted(t, f.other.Create(ctx, &laboratoryv1alpha1.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns}}))
}

// With the L7 proxy on and the defaults (rdp on; terminal and vnc off), http and rdp are admitted and the others are refused.
func TestFeatureAdmissionAccessesUnderTheProxy(t *testing.T) {
	ctx := context.Background()
	f := startFeatureCluster(t, nil, "--set", "vpn.enabled=false")
	ns := f.group(t, "proxyon", nil, nil)
	accepted(t, f.other.Create(ctx, lab(ns, "web", withAccess(laboratoryv1alpha1.AccessHTTP))))
	accepted(t, f.other.Create(ctx, lab(ns, "desk", withAccess(laboratoryv1alpha1.AccessRDP))))
	for _, k := range []laboratoryv1alpha1.AccessKind{laboratoryv1alpha1.AccessTerminal, laboratoryv1alpha1.AccessVNC} {
		refused(t, f.other.Create(ctx, lab(ns, "acc-"+string(k), withAccess(k))), "ACCESS_UNAVAILABLE")
	}
	// the VPN is off but the proxy reads the policy: allowed; a client is refused
	accepted(t, f.other.Create(ctx, &laboratoryv1alpha1.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns}}))
	refused(t, f.other.Create(ctx, &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: ns}, Spec: laboratoryv1alpha1.LabGroupClientSpec{PublicKey: "k"}}), "VPN_UNAVAILABLE")
}
