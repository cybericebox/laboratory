package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func systemScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func pod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g", Labels: labels}}
}

// A device pod is labelled app=<device name> by the platform; a device named like a system component must be neither selected as
// that component nor hold the system selectors back.
func TestSystemSelectorIgnoresDevicePodsNamedLikeComponents(t *testing.T) {
	ctx := context.Background()
	attacker := pod("vpn-x1", map[string]string{"app": "vpn", names.LabelLab: "lab1", names.LabelDevice: "vpn"})

	// Only the device pod and the labelled VPN pod: the labelled selector is used.
	c := fake.NewClientBuilder().WithScheme(systemScheme(t)).WithObjects(attacker,
		pod("vpn-real", map[string]string{"app": "vpn", names.LabelComponent: "vpn"})).Build()
	r := &LabGroupReconciler{Client: c}
	sel, err := r.systemSelector(ctx, "g", names.ComponentVPN)
	if err != nil || len(sel) != 1 || sel[names.LabelComponent] != "vpn" {
		t.Fatalf("selector %v %v: the device pod must not hold the switch back", sel, err)
	}

	// An old VPN pod without the label: the old selector stays until it is rolled.
	c = fake.NewClientBuilder().WithScheme(systemScheme(t)).WithObjects(attacker, pod("vpn-old", map[string]string{"app": "vpn"})).Build()
	r = &LabGroupReconciler{Client: c}
	sel, err = r.systemSelector(ctx, "g", names.ComponentVPN)
	if err != nil || sel["app"] != "vpn" || len(sel) != 1 {
		t.Fatalf("selector %v %v: a pod made before the label keeps working", sel, err)
	}
}

func TestVPNServiceSelectorMovesWhenPodsCarryTheLabel(t *testing.T) {
	ctx := context.Background()
	old := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: "g"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "vpn"}}}
	c := fake.NewClientBuilder().WithScheme(systemScheme(t)).WithObjects(old, pod("vpn-old", map[string]string{"app": "vpn"})).Build()
	r := &LabGroupReconciler{Client: c}
	get := func() map[string]string {
		var s corev1.Service
		if err := c.Get(ctx, types.NamespacedName{Name: "vpn", Namespace: "g"}, &s); err != nil {
			t.Fatal(err)
		}
		return s.Spec.Selector
	}
	if err := r.ensureVPNService(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	if got := get(); got["app"] != "vpn" {
		t.Fatalf("while the pod is unlabelled the Service must keep selecting it: %v", got)
	}
	// The pod is rolled: the new one carries the label.
	if err := c.Delete(ctx, pod("vpn-old", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, pod("vpn-new", map[string]string{"app": "vpn", names.LabelComponent: "vpn"})); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureVPNService(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	if got := get(); len(got) != 1 || got[names.LabelComponent] != "vpn" {
		t.Fatalf("the Service must select by the component label: %v", got)
	}
}

func TestIsVPNPodAndSystemComponent(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		vpn    bool
		comp   string
	}{
		{"labelled vpn", map[string]string{names.LabelComponent: "vpn", "app": "vpn"}, true, "vpn"},
		{"old vpn", map[string]string{"app": "vpn"}, true, "vpn"},
		{"old gateway", map[string]string{"app": "gateway"}, false, "gateway"},
		{"device named vpn", map[string]string{"app": "vpn", names.LabelLab: "l", names.LabelDevice: "vpn"}, false, "vpn"},
		{"labelled gateway wins over app", map[string]string{names.LabelComponent: "gateway", "app": "vpn"}, false, "gateway"},
		{"unknown component", map[string]string{names.LabelComponent: "x", "app": "vpn"}, false, ""},
	}
	for _, c := range cases {
		p := pod("p", c.labels)
		if got := isVPNPod(p); got != c.vpn {
			t.Errorf("%s: isVPNPod = %v", c.name, got)
		}
		// the scheduler tests device pods first, so only pods without the lab label reach this
		if _, isDevice := c.labels[names.LabelLab]; !isDevice {
			if got := systemComponentOf(p); got != c.comp {
				t.Errorf("%s: systemComponentOf = %q, want %q", c.name, got, c.comp)
			}
		}
	}
}

func TestSetComponentLabel(t *testing.T) {
	var tpl corev1.PodTemplateSpec
	if !setComponentLabel(&tpl, "vpn") || tpl.Labels[names.LabelComponent] != "vpn" {
		t.Fatal("the label is added")
	}
	if setComponentLabel(&tpl, "vpn") {
		t.Fatal("no change the second time (no needless rollout)")
	}
}

// A tenant cannot set the component label: it carries the platform prefix.
func TestComponentLabelIsReserved(t *testing.T) {
	if !names.IsReservedLabel(names.LabelComponent) {
		t.Fatal("tenant labels must not reach LabelComponent")
	}
	if got := names.UserLabels(map[string]string{names.LabelComponent: "vpn", "team": "a"}); len(got) != 1 {
		t.Fatalf("user labels: %v", got)
	}
}

// R-10: the proxy may write its traffic reports only in the group namespaces, through a RoleBinding the operator makes there.
func TestProxyReportsBindingIsMadeInTheGroupNamespace(t *testing.T) {
	ctx := context.Background()
	s := systemScheme(t)
	_ = rbacv1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	off := &LabGroupReconciler{Client: c}
	if err := off.ensureProxyReportsBinding(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	var list rbacv1.RoleBindingList
	_ = c.List(ctx, &list)
	if len(list.Items) != 0 {
		t.Fatal("no proxy, no binding")
	}
	on := &LabGroupReconciler{Client: c, ProxyEnabled: true}
	for i := 0; i < 2; i++ { // idempotent
		if err := on.ensureProxyReportsBinding(ctx, "g"); err != nil {
			t.Fatal(err)
		}
	}
	var rb rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Name: names.ProxyReportsBindingName, Namespace: "g"}, &rb); err != nil {
		t.Fatal(err)
	}
	if rb.RoleRef.Name != names.RoleProxyReportsName || rb.RoleRef.Kind != "ClusterRole" ||
		len(rb.Subjects) != 1 || rb.Subjects[0].Name != "laboratory-proxy" || rb.Subjects[0].Namespace != names.ProxyNamespace {
		t.Fatalf("%+v", rb)
	}
}
