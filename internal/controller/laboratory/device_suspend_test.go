package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func TestLabGroupSuspendedFindsGroupOfPrefixedNamespace(t *testing.T) {
	scheme := pruneScheme(t)
	_ = corev1.AddToScheme(scheme)
	group := &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec:       laboratoryv1alpha1.LabGroupSpec{Suspended: true},
	}
	other := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}}
	nsA := laboratoryv1alpha1.LabGroupNamespace(group.Name)
	nsB := laboratoryv1alpha1.LabGroupNamespace(other.Name)
	byStatus := &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "team-c"},
		Spec:       laboratoryv1alpha1.LabGroupSpec{Suspended: true},
		Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: "lg-team-c-status"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		group, other, byStatus,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsA, Labels: map[string]string{names.LabelGroup: group.Name}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsB, Labels: map[string]string{names.LabelGroup: other.Name}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lg-team-c-status"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "plain"}},
	).Build()
	r := &DeviceReconciler{Client: c, Scheme: scheme}

	for ns, want := range map[string]bool{nsA: true, nsB: false, "lg-team-c-status": true, "plain": false, "missing": false} {
		got, err := r.labGroupSuspended(context.Background(), ns)
		if err != nil {
			t.Fatalf("%s: %v", ns, err)
		}
		if got != want {
			t.Errorf("%s: suspended=%v want %v", ns, got, want)
		}
	}
}
