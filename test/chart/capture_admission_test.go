package chart_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// Real API-server admission must confine the new patch privilege to the bound
// node's Device-owned Pod annotation, including removal for invalidation.
func TestNodeAgentCaptureAdmission(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/node-agent/admission-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: laboratory-node-agent-capture") {
		t.Fatal("capture admission policy absent")
	}
	dir := os.Getenv("KUBEBUILDER_ASSETS")
	if dir == "" {
		dirs, _ := filepath.Glob("../../bin/k8s/*")
		if len(dirs) > 0 {
			dir = dirs[0]
		}
	}
	env := &envtest.Environment{BinaryAssetsDirectory: dir}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start admission API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	admin, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		var obj unstructured.Unstructured
		if yaml.Unmarshal([]byte(doc), &obj.Object) != nil || obj.Object == nil {
			continue
		}
		if err := admin.Create(ctx, &obj); err != nil {
			t.Fatal(err)
		}
	}
	const user = "system:serviceaccount:laboratory-system:laboratory-node-agent"
	if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "capture-test"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}); err != nil {
		t.Fatal(err)
	}
	imp := rest.CopyConfig(cfg)
	imp.Impersonate = rest.ImpersonationConfig{UserName: user, Extra: map[string][]string{"authentication.kubernetes.io/node-name": {"node-a"}}}
	agent, err := client.New(imp, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	controller := true
	for _, name := range []string{"own", "other", "plain"} {
		node := "node-a"
		if name == "other" {
			node = "node-b"
		}
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: map[string]string{"laboratory.cybericebox.com/state-device": "device"}}, Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Image: "example:1"}}}}
		if name != "plain" {
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "laboratory.cybericebox.com/v1alpha1", Kind: "Device", Name: "device", UID: types.UID("device-u"), Controller: &controller}}
		}
		if err := admin.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	mutate := func(name string, f func(*corev1.Pod)) error {
		var p corev1.Pod
		if err := admin.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &p); err != nil {
			return err
		}
		before := p.DeepCopy()
		f(&p)
		return agent.Patch(ctx, &p, client.MergeFrom(before))
	}
	guard := func(p *corev1.Pod) { p.Annotations["laboratory.cybericebox.com/capture-guard"] = "held" }
	deadline := time.Now().Add(20 * time.Second)
	for {
		err = mutate("other", guard)
		if apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("other-node write not denied: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, tc := range []struct {
		name, pod string
		edit      func(*corev1.Pod)
	}{
		{"own", "own", guard}, {"remove", "own", func(p *corev1.Pod) { delete(p.Annotations, "laboratory.cybericebox.com/capture-guard") }},
		{"other", "other", guard}, {"plain", "plain", guard}, {"spec", "own", func(p *corev1.Pod) { p.Spec.Containers[0].Image = "other:2" }},
		{"unrelated annotation", "own", func(p *corev1.Pod) { p.Annotations["evil"] = "x" }}, {"label", "own", func(p *corev1.Pod) { p.Labels = map[string]string{"evil": "x"} }},
		{"generateName", "own", func(p *corev1.Pod) { p.GenerateName = "changed-" }},
		{"owner", "own", func(p *corev1.Pod) { p.OwnerReferences = nil }}, {"finalizer", "own", func(p *corev1.Pod) { p.Finalizers = []string{"example.com/evil"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := mutate(tc.pod, tc.edit)
			if tc.name == "own" || tc.name == "remove" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !apierrors.IsForbidden(err) {
				t.Fatalf("mutation not forbidden: %v", err)
			}
		})
	}
	// Bound node identity is mandatory even when RBAC permits the patch.
	missingCfg := rest.CopyConfig(cfg)
	missingCfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	missing, err := client.New(missingCfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	bound := agent
	agent = missing
	err = mutate("own", guard)
	agent = bound
	if !apierrors.IsForbidden(err) {
		t.Fatalf("missing bound-node identity accepted: %v", err)
	}
	var statusPod corev1.Pod
	if err := admin.Get(ctx, types.NamespacedName{Namespace: "default", Name: "own"}, &statusPod); err != nil {
		t.Fatal(err)
	}
	beforeStatus := statusPod.DeepCopy()
	statusPod.Status.Phase = corev1.PodRunning
	if err := agent.Status().Patch(ctx, &statusPod, client.MergeFrom(beforeStatus)); !apierrors.IsForbidden(err) {
		t.Fatalf("Pod status mutation accepted: %v", err)
	}
	// The annotation RV fence defeats an in-flight deletion that saw the old guard.
	var p corev1.Pod
	key := types.NamespacedName{Namespace: "default", Name: "own"}
	if err := admin.Get(ctx, key, &p); err != nil {
		t.Fatal(err)
	}
	rv := p.ResourceVersion
	uid := p.UID
	if err := mutate("own", guard); err != nil {
		t.Fatal(err)
	}
	if err := admin.Delete(ctx, &p, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); !apierrors.IsConflict(err) {
		t.Fatalf("stale guard deletion accepted: %v", err)
	}
}
