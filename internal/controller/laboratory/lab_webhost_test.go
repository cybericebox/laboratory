package laboratory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func webLab(name, uid string) *laboratoryv1alpha1.Lab {
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-alpha", UID: types.UID(uid)}}
	lab.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{{
		Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer,
		Exposure: &laboratoryv1alpha1.ExposureSpec{Web: &laboratoryv1alpha1.WebExposure{Port: 80}},
	}}
	return lab
}

func webServices(t *testing.T, c client.Client) []corev1.Service {
	t.Helper()
	var list corev1.ServiceList
	if err := c.List(context.Background(), &list, client.InNamespace("team-alpha")); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func scriptedCodes(codes ...string) (func(int) (string, error), *[]int) {
	var lens []int
	i := 0
	return func(n int) (string, error) {
		lens = append(lens, n)
		if i >= len(codes) {
			return "", errors.New("script exhausted")
		}
		c := codes[i]
		i++
		return c, nil
	}, &lens
}

func TestEnsureWebServicesStableAcrossReconciles(t *testing.T) {
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &LabReconciler{Client: c, Scheme: s, BaseDomain: "labs.example.com"}
	lab := webLab("lab1", "uid-1")
	ctx := context.Background()

	if err := r.ensureWebServices(ctx, lab); err != nil {
		t.Fatal(err)
	}
	svcs := webServices(t, c)
	if len(svcs) != 1 || !regexp.MustCompile(`^web-[a-z0-9]{3}$`).MatchString(svcs[0].Name) {
		t.Fatalf("services: %+v", svcs)
	}
	first := svcs[0].Name
	if svcs[0].Labels[names.LabelLab] != "lab1" || svcs[0].Labels[names.LabelDevice] != "web" {
		t.Fatalf("labels: %v", svcs[0].Labels)
	}

	r.newWebCode = func(int) (string, error) { return "zzz", nil } // must not be used again
	for i := 0; i < 3; i++ {
		if err := r.ensureWebServices(ctx, lab); err != nil {
			t.Fatal(err)
		}
	}
	svcs = webServices(t, c)
	if len(svcs) != 1 || svcs[0].Name != first {
		t.Fatalf("name changed: %+v", svcs)
	}

	access := r.buildAccessEntries(ctx, lab)
	if len(access) != 1 || access[0].URL != fmt.Sprintf("https://%s.labs.example.com", first) {
		t.Fatalf("access: %+v", access)
	}
}

func TestEnsureWebServicesRetriesOnConflict(t *testing.T) {
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	other := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web-aaa", Namespace: "team-alpha",
		Labels: map[string]string{names.LabelLab: "other", names.LabelDevice: "web"}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(other).Build()
	draw, lens := scriptedCodes("aaa", "aaa", "bbb")
	r := &LabReconciler{Client: c, Scheme: s, newWebCode: draw}

	if err := r.ensureWebServices(context.Background(), webLab("lab1", "uid-1")); err != nil {
		t.Fatal(err)
	}
	var got corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Name: "web-bbb", Namespace: "team-alpha"}, &got); err != nil {
		t.Fatalf("web-bbb missing: %v", err)
	}
	if got.Labels[names.LabelLab] != "lab1" {
		t.Fatalf("labels: %v", got.Labels)
	}
	// the other lab's Service is untouched
	var kept corev1.Service
	_ = c.Get(context.Background(), types.NamespacedName{Name: "web-aaa", Namespace: "team-alpha"}, &kept)
	if kept.Labels[names.LabelLab] != "other" {
		t.Fatalf("foreign service modified: %v", kept.Labels)
	}
	if len(*lens) != 3 {
		t.Fatalf("draws: %v", *lens)
	}
}

func TestEnsureWebServicesWidensToFourCharsAfterEightConflicts(t *testing.T) {
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	var taken []client.Object
	codes := []string{}
	for i := 0; i < names.WebCodeAttempts; i++ {
		code := fmt.Sprintf("c%02d", i)
		codes = append(codes, code)
		taken = append(taken, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web-" + code, Namespace: "team-alpha"}})
	}
	codes = append(codes, "wxyz")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(taken...).Build()
	draw, lens := scriptedCodes(codes...)
	r := &LabReconciler{Client: c, Scheme: s, newWebCode: draw}

	if err := r.ensureWebServices(context.Background(), webLab("lab1", "uid-1")); err != nil {
		t.Fatal(err)
	}
	want := append(make([]int, 0), 3, 3, 3, 3, 3, 3, 3, 3, 4)
	if fmt.Sprint(*lens) != fmt.Sprint(want) {
		t.Fatalf("code lengths = %v, want %v", *lens, want)
	}
	var got corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Name: "web-wxyz", Namespace: "team-alpha"}, &got); err != nil {
		t.Fatalf("web-wxyz missing: %v", err)
	}
}

func TestEnsureWebServicesGivesUpAfterAllAttempts(t *testing.T) {
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web-aaaa", Namespace: "team-alpha"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web-aaa", Namespace: "team-alpha"}},
	).Build()
	r := &LabReconciler{Client: c, Scheme: s, newWebCode: func(n int) (string, error) { return "aaaa"[:n], nil }}
	if err := r.ensureWebServices(context.Background(), webLab("lab1", "uid-1")); err == nil {
		t.Fatal("expected an error")
	}
}
