package chart_test

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const (
	groupLabel      = "laboratory.cybericebox.com/group"
	networksAnn     = "network.cybericebox.com/networks"
	defaultNetAnn   = "network.cybericebox.com/default-network"
	conntrackAnn    = "network.cybericebox.com/conntrack-accounting"
	operatorUser    = "system:serviceaccount:laboratory-system:laboratory-controller-manager"
	somebodyElse    = "system:serviceaccount:default:somebody"
	admissionPolicy = "templates/operator/lab-namespace-policy.yaml"
)

// The boundary of cni-gate (lg-* namespaces go to the node-agent, the rest does not) against a real API server: only the operator labels a
// namespace as a LabGroup's, and only with the lg- name; a pod (or a workload template) with a network annotation lives only in lg-*.
func TestLabNamespaceAdmission(t *testing.T) {
	out, err := helmTemplate(t, "-s", admissionPolicy)
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: envtestDir()}
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
	as := func(user string) client.Client {
		if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "t-" + strings.ReplaceAll(strings.ReplaceAll(user, ":", "-"), "_", "-")}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		imp := rest.CopyConfig(cfg)
		imp.Impersonate = rest.ImpersonationConfig{UserName: user}
		c, err := client.New(imp, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	operator, other := as(operatorUser), as(somebodyElse)
	ns := func(name string, labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	// wait until the policies are enforced
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; ; i++ {
		if err := other.Create(ctx, ns("probe-"+string(rune('a'+i%26))+time.Now().Format("150405.000")[7:], map[string]string{groupLabel: "x"})); apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the policies are not enforced")
		}
		time.Sleep(200 * time.Millisecond)
	}
	forbid := func(t *testing.T, err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) {
			t.Fatalf("must be forbidden: %v", err)
		}
	}
	allow := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("must be allowed: %v", err)
		}
	}

	t.Run("namespace label", func(t *testing.T) {
		allow(t, operator.Create(ctx, ns("lg-ok-0123456789ab", map[string]string{groupLabel: "ok"})))
		forbid(t, operator.Create(ctx, ns("kube-evil", map[string]string{groupLabel: "evil"})))
		forbid(t, other.Create(ctx, ns("lg-stolen-0123456789ab", map[string]string{groupLabel: "stolen"})))
		allow(t, other.Create(ctx, ns("plain", nil)))
		allow(t, other.Create(ctx, ns("lg-plain-without-label", nil)))
		// labelling or relabelling a foreign namespace
		var plain corev1.Namespace
		allow(t, admin.Get(ctx, client.ObjectKey{Name: "plain"}, &plain))
		plain.Labels = map[string]string{groupLabel: "x"}
		forbid(t, other.Update(ctx, &plain))
		forbid(t, operator.Update(ctx, &plain)) // not an lg-* name
		var ok corev1.Namespace
		allow(t, admin.Get(ctx, client.ObjectKey{Name: "lg-ok-0123456789ab"}, &ok))
		ok.Labels[groupLabel] = "other"
		forbid(t, other.Update(ctx, &ok))
		delete(ok.Labels, groupLabel)
		forbid(t, other.Update(ctx, &ok))
		// other labels of the group's namespace stay writable by everyone; the group label is untouched
		allow(t, admin.Get(ctx, client.ObjectKey{Name: "lg-ok-0123456789ab"}, &ok))
		ok.Labels["pod-security.kubernetes.io/enforce"] = "baseline"
		allow(t, other.Update(ctx, &ok))
		allow(t, operator.Update(ctx, &ok))
	})

	pod := func(namespace string, ann map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "p-", Namespace: namespace, Annotations: ann}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}
	}
	template := func(ann map[string]string) corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "b"}, Annotations: ann}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}
	}
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
	t.Run("pods with a network annotation", func(t *testing.T) {
		allow(t, admin.Create(ctx, ns("lg-pods-0123456789ab", nil)))
		allow(t, admin.Create(ctx, ns("workloads", nil)))
		for _, key := range []string{networksAnn, defaultNetAnn, conntrackAnn} {
			ann := map[string]string{key: "x"}
			allow(t, other.Create(ctx, pod("lg-pods-0123456789ab", ann)))
			forbid(t, other.Create(ctx, pod("workloads", ann)))
			forbid(t, other.Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d-" + strings.ToLower(key[len(key)-4:]), Namespace: "workloads"}, Spec: appsv1.DeploymentSpec{Selector: sel, Template: template(ann)}}))
		}
		allow(t, other.Create(ctx, pod("workloads", map[string]string{"unrelated": "x"})))
		allow(t, other.Create(ctx, pod("workloads", nil)))
		// an empty value counts too: default-network "" means a stub eth0 for the node-agent
		forbid(t, other.Create(ctx, pod("workloads", map[string]string{defaultNetAnn: ""})))
		ann := map[string]string{networksAnn: "eth1@net"}
		allow(t, other.Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: "lg-pods-0123456789ab"}, Spec: appsv1.DeploymentSpec{Selector: sel, Template: template(ann)}}))
		forbid(t, other.Create(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "ss", Namespace: "workloads"}, Spec: appsv1.StatefulSetSpec{Selector: sel, ServiceName: "s", Template: template(ann)}}))
		forbid(t, other.Create(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: "workloads"}, Spec: appsv1.DaemonSetSpec{Selector: sel, Template: template(ann)}}))
		jobTemplate := template(ann)
		jobTemplate.Spec.RestartPolicy = corev1.RestartPolicyNever
		forbid(t, other.Create(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "workloads"}, Spec: batchv1.JobSpec{Template: jobTemplate}}))
		forbid(t, other.Create(ctx, &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "cj", Namespace: "workloads"}, Spec: batchv1.CronJobSpec{Schedule: "* * * * *", JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: jobTemplate}}}}))
		allow(t, other.Create(ctx, &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "cj-ok", Namespace: "lg-pods-0123456789ab"}, Spec: batchv1.CronJobSpec{Schedule: "* * * * *", JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: jobTemplate}}}}))
		allow(t, other.Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "workloads"}, Spec: appsv1.DeploymentSpec{Selector: sel, Template: template(nil)}}))
	})
}
