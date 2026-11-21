package chart_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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

// The verbs the code really uses on the upstream Secrets: the agent deletes and creates a lab's Secret (no get, no update, no patch, no list);
// the operator writes the tenant and platform Secrets and deletes lab ones.
func TestUpstreamRolesCarryOnlyTheVerbsTheCodeUses(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/upstream-namespace.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := map[string]string{
		"laboratory-agent-upstream":    "create,delete",
		"laboratory-operator-upstream": "create,delete,get,list,update",
	}
	for name, verbs := range want {
		role := ofKind(docs(t, out), "Role")[name]
		if role == nil {
			t.Fatalf("no Role %s", name)
		}
		rs := rulesOf(t, role)
		if len(rs) != 1 || !has(rs[0].Resources, "secrets") {
			t.Fatalf("%s: one rule on Secrets: %+v", name, rs)
		}
		got := append([]string{}, rs[0].Verbs...)
		sortStrings(got)
		if strings.Join(got, ",") != verbs {
			t.Errorf("%s verbs %v, want %s", name, got, verbs)
		}
		if len(rs[0].ResourceNames) != 0 {
			t.Errorf("%s: names are bounded by the admission policy, not here: %v", name, rs[0].ResourceNames)
		}
	}
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// The names the two writers may touch, enforced by the API server: the agent creates and deletes lab-* and nothing else; the operator writes
// tenant-* and platform and deletes those and lab-*.
func TestUpstreamWritersPolicyBoundsTheNames(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/upstream-admission-policy.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
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
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "laboratory-upstream"}}); err != nil {
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
	const agentUser = "system:serviceaccount:laboratory-agent:laboratory-agent"
	const operatorUser = "system:serviceaccount:laboratory-system:laboratory-controller-manager"
	as := func(user string) client.Client {
		if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "up-" + strings.ReplaceAll(user, ":", "-")}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}); err != nil {
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
	agent, operator := as(agentUser), as(operatorUser)
	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "laboratory-upstream"}, StringData: map[string]string{"k": "v"}}
	}
	// wait until the policy is enforced
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := agent.Create(ctx, secret("tenant-probe"))
		if apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the policy is not enforced: %v", err)
		}
		_ = admin.Delete(ctx, secret("tenant-probe"))
		time.Sleep(100 * time.Millisecond)
	}
	create := func(c client.Client, name string) error { return c.Create(ctx, secret(name)) }
	update := func(c client.Client, name string) error {
		s := secret(name)
		s.Data = map[string][]byte{"k": []byte("w")}
		return c.Update(ctx, s)
	}
	del := func(c client.Client, name string) error { return c.Delete(ctx, secret(name)) }
	// seed what the delete cases need, as the cluster admin (no policy rule applies to it)
	for _, n := range []string{"lab-seed1", "lab-seed2", "lab-seed3", "tenant-seed", "platform", "other"} {
		if err := admin.Create(ctx, secret(n)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		do   func() error
		ok   bool
	}{
		{"agent creates lab-x", func() error { return create(agent, "lab-x") }, true},
		{"agent deletes lab-x", func() error { return del(agent, "lab-x") }, true},
		{"agent deletes lab-seed1", func() error { return del(agent, "lab-seed1") }, true},
		{"agent cannot create tenant-x", func() error { return create(agent, "tenant-x") }, false},
		{"agent cannot create platform", func() error { return create(agent, "platform") }, false},
		{"agent cannot create another name", func() error { return create(agent, "other-x") }, false},
		{"agent cannot update lab-seed2", func() error { return update(agent, "lab-seed2") }, false},
		{"agent cannot update platform", func() error { return update(agent, "platform") }, false},
		{"agent cannot delete tenant-seed", func() error { return del(agent, "tenant-seed") }, false},
		{"agent cannot delete platform", func() error { return del(agent, "platform") }, false},
		{"operator creates tenant-y", func() error { return create(operator, "tenant-y") }, true},
		{"operator updates tenant-seed", func() error { return update(operator, "tenant-seed") }, true},
		{"operator updates platform", func() error { return update(operator, "platform") }, true},
		{"operator deletes tenant-y", func() error { return del(operator, "tenant-y") }, true},
		{"operator deletes lab-seed2", func() error { return del(operator, "lab-seed2") }, true},
		{"operator cannot create lab-z", func() error { return create(operator, "lab-z") }, false},
		{"operator cannot update lab-seed3", func() error { return update(operator, "lab-seed3") }, false},
		{"operator cannot create another name", func() error { return create(operator, "other-y") }, false},
		{"operator cannot delete another name", func() error { return del(operator, "other") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.do()
			switch {
			case tc.ok && err != nil:
				t.Fatalf("must be allowed: %v", err)
			case !tc.ok && !apierrors.IsForbidden(err):
				t.Fatalf("must be forbidden: %v", err)
			}
		})
	}
}

// The policy is a value-controlled part of the chart: it is not rendered when the operator's admission policies are off.
func TestUpstreamWritersPolicyFollowsTheAdmissionSwitch(t *testing.T) {
	out, err := helmTemplate(t, "--set", "operator.admissionPolicy.enabled=false", "-s", "templates/upstream-admission-policy.yaml")
	if err == nil && strings.Contains(out, "ValidatingAdmissionPolicy") {
		t.Errorf("a disabled policy must not render:\n%s", out)
	}
}

// The registry pod: no token by default (pod level), the gate container alone mounts a projected one (with the CA and the namespace); zot mounts none.
func TestRegistryPodGivesTheTokenToTheGateOnly(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/registry/deployment.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var dep appsv1.Deployment
	if err := yaml.Unmarshal([]byte(out), &dep); err != nil {
		t.Fatal(err)
	}
	spec := dep.Spec.Template.Spec
	if spec.ServiceAccountName != "laboratory-registry-gate" {
		t.Errorf("service account %q", spec.ServiceAccountName)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatalf("the pod must not automount the token: %v", spec.AutomountServiceAccountToken)
	}
	var vol *corev1.Volume
	for i, v := range spec.Volumes {
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.ServiceAccountToken != nil {
					vol = &spec.Volumes[i]
				}
			}
		}
	}
	if vol == nil {
		t.Fatalf("no projected token volume: %v", spec.Volumes)
	}
	const tokenDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	var gate, zot *corev1.Container
	for i, c := range spec.Containers {
		switch c.Name {
		case "gate":
			gate = &spec.Containers[i]
		default:
			zot = &spec.Containers[i]
		}
	}
	if gate == nil || zot == nil || len(spec.Containers) != 2 {
		t.Fatalf("expected the gate and zot: %v", spec.Containers)
	}
	var mounted bool
	for _, m := range gate.VolumeMounts {
		if m.Name == vol.Name {
			mounted = m.MountPath == tokenDir && m.ReadOnly
		}
	}
	if !mounted {
		t.Errorf("the gate mounts the projected token read-only at %s: %v", tokenDir, gate.VolumeMounts)
	}
	for _, m := range zot.VolumeMounts {
		if m.Name == vol.Name || strings.HasPrefix(m.MountPath, "/var/run/secrets") {
			t.Errorf("zot must not see the token: %+v", m)
		}
	}
	for _, c := range spec.InitContainers {
		for _, m := range c.VolumeMounts {
			if m.Name == vol.Name {
				t.Errorf("init container %s mounts the token", c.Name)
			}
		}
	}
	// the gate learns the node-local image host from its environment, always
	var host string
	for _, e := range gate.Env {
		if e.Name == "GATE_IMAGE_HOST" {
			host = e.Value
		}
	}
	if host != "localhost:5035" {
		t.Errorf("GATE_IMAGE_HOST %q", host)
	}
}

// Only the operator creates ImagePull objects; the node-agent answers them, the gate and the scheduler read.
func TestOnlyTheOperatorCreatesImagePulls(t *testing.T) {
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/operator/clusterrole.yaml", "-s", "templates/node-agent/clusterrole.yaml", "-s", "templates/registry/rbac.yaml",
		"-s", "templates/agent/clusterrole.yaml", "-s", "templates/lab-scheduler/scheduler.yaml", "--set", "scheduler.enabled=true")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var creators []string
	for _, d := range docs(t, out) {
		if d["kind"] != "ClusterRole" && d["kind"] != "Role" {
			continue
		}
		name := d["metadata"].(map[string]any)["name"].(string)
		for _, r := range rulesOf(t, d) {
			if (has(r.Resources, "imagepulls") || has(r.Resources, "*")) && (has(r.Verbs, "create") || has(r.Verbs, "*")) {
				creators = append(creators, name)
			}
		}
	}
	if len(creators) != 1 || !strings.Contains(creators[0], "manager") && !strings.Contains(creators[0], "operator") {
		t.Errorf("only the operator creates ImagePulls: %v", creators)
	}
}

// The gate's rights: the schedule and the admission objects, read only; token reviews; Secrets only in the upstream namespace (rbac test).
func TestRegistryGateRightsAreReadOnlyAndBounded(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/registry/rbac.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cr := ofKind(docs(t, out), "ClusterRole")["laboratory-registry-gate"]
	if cr == nil {
		t.Fatal("no ClusterRole laboratory-registry-gate")
	}
	got := map[string][]string{}
	for _, r := range rulesOf(t, cr) {
		for _, res := range r.Resources {
			got[res] = append(got[res], r.Verbs...)
		}
	}
	if !has(got["tokenreviews"], "create") || len(got["tokenreviews"]) != 1 {
		t.Errorf("tokenreviews create only: %v", got["tokenreviews"])
	}
	for _, res := range []string{"pods", "nodes", "devices", "labgroups", "imagepulls", "tenants", "labs"} {
		v := got[res]
		if !has(v, "list") || !has(v, "watch") || has(v, "create") || has(v, "update") || has(v, "patch") || has(v, "delete") {
			t.Errorf("%s: list/watch read-only expected, got %v", res, v)
		}
	}
	if _, ok := got["secrets"]; ok {
		t.Error("the cluster-wide role must not touch Secrets")
	}
	var role map[string]any
	for _, d := range docs(t, out) {
		if d["kind"] == "Role" {
			role = d
			if d["metadata"].(map[string]any)["namespace"] != "laboratory-upstream" {
				t.Errorf("the gate's Role is in %v", d["metadata"])
			}
		}
	}
	if role == nil {
		t.Fatal("no Role of the gate in the upstream namespace")
	}
	rs := rulesOf(t, role)
	if len(rs) != 1 || !has(rs[0].Resources, "secrets") || !has(rs[0].Verbs, "get") || !has(rs[0].Verbs, "list") || !has(rs[0].Verbs, "watch") || len(rs[0].Verbs) != 3 {
		t.Errorf("secrets get/list/watch in the upstream namespace: %+v", rs)
	}
}

// Every registry of the gate's configuration names the hosts of its token service, so the gate may follow a login redirect to them and to no other host.
func TestRegistryAuthHosts(t *testing.T) {
	out, err := helmTemplate(t, "-s", "templates/registry/gate-configmap.yaml",
		"--set", "registry.cache.extraRegistries[0].name=reg.example.com",
		"--set", "registry.cache.extraRegistries[0].url=https://reg.example.com",
		"--set", "registry.cache.extraRegistries[0].authHosts[0]=auth.example.com")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(out), &cm); err != nil {
		t.Fatal(err)
	}
	var list []struct {
		Name      string   `json:"name"`
		AuthHosts []string `json:"authHosts"`
	}
	if err := json.Unmarshal([]byte(cm.Data["registries.json"]), &list); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, r := range list {
		got[r.Name] = r.AuthHosts
	}
	if len(got["docker.io"]) != 1 || got["docker.io"][0] != "auth.docker.io" {
		t.Errorf("docker.io auth hosts: %v", got["docker.io"])
	}
	if len(got["reg.example.com"]) != 1 || got["reg.example.com"][0] != "auth.example.com" {
		t.Errorf("an extra registry carries its own auth hosts: %v", got)
	}
	if _, ok := got["ghcr.io"]; !ok {
		t.Errorf("registries: %v", got)
	}
}

// The platform's own registry and images are gone from the chart: no value, no agent environment.
func TestNoPlatformOnlyImages(t *testing.T) {
	// the Tenant CRD is generated from the Go types and loses the field with the code (pools-registry)
	noMatch(t, "platformOnly", filepath.Join(chartDir, "templates"), `platformOnly|PLATFORM_ONLY`)
	noMatch(t, "platformOnly", filepath.Join(chartDir, "values.yaml"), `platformOnly|PLATFORM_ONLY`)
	out, err := helmTemplate(t, append(agentSet, "-s", "templates/agent/deployment.yaml")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "PLATFORM_ONLY") {
		t.Errorf("the agent has no platform-only switch:\n%s", out)
	}
}
