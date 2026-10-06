package laboratory

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/devices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func codeLab(name, uid string) *laboratoryv1alpha1.Lab {
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-alpha", UID: types.UID(uid)}}
	lab.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{
		{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Exposure: &laboratoryv1alpha1.ExposureSpec{Web: &laboratoryv1alpha1.WebExposure{Port: 80}}},
		{Name: "db", Type: laboratoryv1alpha1.DeviceTypeContainer},
	}
	return lab
}

func codeReconciler(t *testing.T, objs ...client.Object) (*LabReconciler, client.Client) {
	t.Helper()
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&laboratoryv1alpha1.Device{}).Build()
	return &LabReconciler{Client: c, Scheme: s, BaseDomain: "labs.example.com"}, c
}

func devicesOf(t *testing.T, c client.Client) map[string]laboratoryv1alpha1.Device {
	t.Helper()
	var list laboratoryv1alpha1.DeviceList
	if err := c.List(context.Background(), &list, client.InNamespace("team-alpha")); err != nil {
		t.Fatal(err)
	}
	out := map[string]laboratoryv1alpha1.Device{}
	for _, d := range list.Items {
		out[d.Name] = d
	}
	return out
}

// Two labs with a device of the same name share a namespace: their workloads get
// different names, and no name carries the lab.
func TestDeviceCodesAreUniquePerNamespace(t *testing.T) {
	r, c := codeReconciler(t)
	ctx := context.Background()
	labs := []*laboratoryv1alpha1.Lab{codeLab("alpha-lab", "u1"), codeLab("beta-lab", "u2"), codeLab("gamma-lab", "u3")}
	for _, lab := range labs {
		if err := r.ensureWebServices(ctx, lab); err != nil {
			t.Fatal(err)
		}
		if err := r.materializeDevices(ctx, lab, nil); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]string{}
	devs := devicesOf(t, c)
	for _, lab := range labs {
		for _, dev := range []string{"web", "db"} {
			d := devs[devices.Name(lab.Name, dev)]
			if !regexp.MustCompile(`^[a-z0-9]{3}$`).MatchString(d.Spec.Code) {
				t.Fatalf("%s/%s code %q", lab.Name, dev, d.Spec.Code)
			}
			workload := workloadName(&d)
			if workload != dev+"-"+d.Spec.Code || strings.Contains(workload, "lab") {
				t.Fatalf("workload name %q must be <device>-<code>", workload)
			}
			if other, dup := seen[workload]; dup {
				t.Fatalf("%s and %s/%s share the workload name %s", other, lab.Name, dev, workload)
			}
			seen[workload] = lab.Name + "/" + dev
		}
	}
	// The web Service of a device carries the code of its workload.
	var svcs corev1.ServiceList
	if err := c.List(ctx, &svcs, client.InNamespace("team-alpha")); err != nil {
		t.Fatal(err)
	}
	if len(svcs.Items) != 3 {
		t.Fatalf("services: %d", len(svcs.Items))
	}
	for _, svc := range svcs.Items {
		lab := svc.Labels[names.LabelLab]
		d := devs[devices.Name(lab, "web")]
		if svc.Name != workloadName(&d) {
			t.Fatalf("service %s, web workload %s", svc.Name, workloadName(&d))
		}
	}
}

// A second reconcile keeps every code.
func TestDeviceCodesAreStable(t *testing.T) {
	r, c := codeReconciler(t)
	ctx := context.Background()
	lab := codeLab("alpha-lab", "u1")
	for i := 0; i < 3; i++ {
		if err := r.ensureWebServices(ctx, lab); err != nil {
			t.Fatal(err)
		}
		if err := r.materializeDevices(ctx, lab, nil); err != nil {
			t.Fatal(err)
		}
		r.newWebCode = func(int) (string, error) { return "zzz", nil } // must not be drawn again
	}
	for name, d := range devicesOf(t, c) {
		if d.Spec.Type == laboratoryv1alpha1.DeviceTypeContainer && d.Spec.Code == "zzz" {
			t.Fatalf("%s was given a new code", name)
		}
	}
}

// A code that makes the name collide with another device or a Service is skipped;
// after WebCodeAttempts the code widens to WebCodeMaxLen.
func TestDeviceCodeSkipsTakenNames(t *testing.T) {
	taken := &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: "other-db", Namespace: "team-alpha"},
		Spec:       laboratoryv1alpha1.DeviceSpec{Name: "db", Code: "aaa", LabRef: "other", Type: laboratoryv1alpha1.DeviceTypeContainer},
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "db-bbb", Namespace: "team-alpha"}}
	r, _ := codeReconciler(t, taken, svc)
	draw, lens := scriptedCodes("aaa", "bbb", "ccc")
	r.newWebCode = draw
	lab := codeLab("alpha-lab", "u1")
	code, err := r.newCodeAllocator(lab).codeFor(context.Background(), "db")
	if err != nil || code != "ccc" {
		t.Fatalf("code %q err %v", code, err)
	}
	if len(*lens) != 3 {
		t.Fatalf("draws: %v", *lens)
	}
}

// A device that exists without a code (created before codes) keeps its names.
func TestLegacyDeviceKeepsItsName(t *testing.T) {
	legacy := &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-lab-db", Namespace: "team-alpha"},
		Spec:       laboratoryv1alpha1.DeviceSpec{Name: "db", LabRef: "alpha-lab", Type: laboratoryv1alpha1.DeviceTypeContainer},
	}
	r, _ := codeReconciler(t, legacy)
	code, err := r.newCodeAllocator(codeLab("alpha-lab", "u1")).codeFor(context.Background(), "db")
	if err != nil || code != "" {
		t.Fatalf("code %q err %v", code, err)
	}
	if got := workloadName(legacy); got != "alpha-lab-db" {
		t.Fatalf("legacy workload name = %q", got)
	}
}

func TestUserLabels(t *testing.T) {
	in := map[string]string{
		"team": "red", "env": "prod", "app": "mine", "pod-template-hash": "x",
		names.LabelLab: "lab", names.LabelDeployGroup: "g1", names.LabelPrefix + "anything": "y",
	}
	got := userLabels(in)
	if len(got) != 2 || got["team"] != "red" || got["env"] != "prod" {
		t.Fatalf("user labels = %v", got)
	}
}

func TestApplyUserLabelsAddsUpdatesAndRemoves(t *testing.T) {
	obj := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "vpn", names.LabelLab: "l"}}}
	if !applyUserLabels(obj, map[string]string{"team": "red", "env": "prod"}) {
		t.Fatal("first apply changes the object")
	}
	if obj.Labels["team"] != "red" || obj.Labels["env"] != "prod" || obj.Labels["app"] != "vpn" || obj.Annotations[names.AnnotationUserLabels] != "env,team" {
		t.Fatalf("labels %v annotations %v", obj.Labels, obj.Annotations)
	}
	if applyUserLabels(obj, map[string]string{"team": "red", "env": "prod"}) {
		t.Fatal("the same labels change nothing")
	}
	if !applyUserLabels(obj, map[string]string{"team": "blue"}) {
		t.Fatal("update and removal change the object")
	}
	if obj.Labels["team"] != "blue" || obj.Labels["env"] != "" || obj.Labels["app"] != "vpn" || obj.Labels[names.LabelLab] != "l" {
		t.Fatalf("labels %v", obj.Labels)
	}
	if !applyUserLabels(obj, nil) {
		t.Fatal("dropping the last label changes the object")
	}
	if _, has := obj.Labels["team"]; has || obj.Annotations[names.AnnotationUserLabels] != "" {
		t.Fatalf("labels %v annotations %v", obj.Labels, obj.Annotations)
	}
	// Labels the operator did not copy are never removed.
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"mine": "keep"}}}
	applyUserLabels(other, nil)
	if other.Labels["mine"] != "keep" {
		t.Fatal("a foreign label was removed")
	}
}
