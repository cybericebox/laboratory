package l7

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func TestServiceAttribution(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	ns := laboratoryv1alpha1.LabGroupNamespace("g1")
	svc := func(name, namespace string, labels map[string]string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		svc("web-k3x", ns, map[string]string{names.LabelLab: "lab1", names.LabelDevice: "web"}),
		svc("vpn", ns, nil),
		svc("web-q7z", laboratoryv1alpha1.LabGroupNamespace("g2"), map[string]string{names.LabelLab: "lab9", names.LabelDevice: "web"}),
	).Build()
	attribute := ServiceAttribution(c)

	if lab, ok := attribute("web-k3x", "g1"); !ok || lab != "lab1" {
		t.Fatalf("own web service = %q %v", lab, ok)
	}
	for _, tc := range []struct{ host, group string }{
		{"web-q7z", "g1"}, // another group's service
		{"vpn", "g1"},     // not a web service
		{"web-zzz", "g1"}, // no such service
	} {
		if lab, ok := attribute(tc.host, tc.group); ok {
			t.Errorf("attribute(%s, %s) = %q, want none", tc.host, tc.group, lab)
		}
	}
}
