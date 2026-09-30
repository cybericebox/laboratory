package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const statePath = "devices.statePersistence."

func render(t *testing.T, template string, into any, extra ...string) {
	t.Helper()
	out, err := helmTemplate(t, append(extra, "-s", template)...)
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", template, err, out)
	}
	if err := yaml.Unmarshal([]byte(out), into); err != nil {
		t.Fatalf("decode %s: %v\n%s", template, err, out)
	}
}

func envOf(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func TestStatePersistenceOffByDefault(t *testing.T) {
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, unwanted := range []string{"laboratory-snapshots", "STATE_REGISTRY", "STATE_PERSISTENCE", "containerd-root", "DAC_READ_SEARCH"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("with state persistence off the chart must render nothing about it, found %q", unwanted)
		}
	}
	var cm corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &cm)
	for k := range cm.Data {
		if strings.HasPrefix(k, "STATE_") {
			t.Errorf("operator config has %s although the feature is off", k)
		}
	}
}

func TestStatePersistenceRendersRegistryAndWiring(t *testing.T) {
	on := []string{"--set", statePath + "enabled=true"}

	var dep appsv1.Deployment
	render(t, "templates/snapshots/deployment.yaml", &dep, on...)
	zot := dep.Spec.Template.Spec.Containers[0]
	if zot.Image != "ghcr.io/project-zot/zot:v2.1.21" {
		t.Errorf("registry image %q", zot.Image)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("a ReadWriteOnce volume needs the Recreate strategy")
	}

	var pvc corev1.PersistentVolumeClaim
	render(t, "templates/snapshots/pvc.yaml", &pvc, on...)
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "20Gi" {
		t.Errorf("default volume size %s", got)
	}
	if pvc.Spec.StorageClassName != nil {
		t.Errorf("an empty storageClass must leave the cluster default, got %q", *pvc.Spec.StorageClassName)
	}
	if pvc.Annotations["helm.sh/resource-policy"] != "keep" {
		t.Errorf("the snapshot volume must survive an uninstall")
	}

	var svc corev1.Service
	render(t, "templates/snapshots/service.yaml", &svc, on...)
	if svc.Name != "laboratory-snapshots" || svc.Spec.Ports[0].Port != 5000 {
		t.Errorf("service %+v", svc.Spec)
	}

	var secret corev1.Secret
	render(t, "templates/snapshots/secret.yaml", &secret, on...)
	if secret.StringData["username"] != "writer" || len(secret.StringData["password"]) < 32 || !strings.HasPrefix(secret.StringData["htpasswd"], "writer:$2") {
		t.Errorf("registry credentials not generated: %+v", secret.StringData)
	}

	var zotConfig corev1.ConfigMap
	render(t, "templates/snapshots/configmap.yaml", &zotConfig, on...)
	cfg := zotConfig.Data["config.json"]
	for _, want := range []string{`"gc": true`, `"anonymousPolicy": ["read"]`, `"rootDirectory": "/var/lib/registry"`, `"gcInterval": "1h"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("zot config lacks %s:\n%s", want, cfg)
		}
	}

	var opCfg corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &opCfg, on...)
	wantCfg := map[string]string{
		"STATE_PERSISTENCE_ENABLED": "true",
		"STATE_REGISTRY_ADDR":       "laboratory-snapshots.laboratory-system.svc:5000",
		"STATE_DEBOUNCE":            "5s",
		"STATE_EXCLUDE_PATHS":       "/tmp,/var/tmp,/run",
		"STATE_MAX_SNAPSHOT_SIZE":   "512Mi",
		"STATE_MAX_LAYERS":          "10",
		"STATE_RETENTION":           "168h",
	}
	for k, v := range wantCfg {
		if opCfg.Data[k] != v {
			t.Errorf("operator config %s = %q, want %q", k, opCfg.Data[k], v)
		}
	}

	var op appsv1.Deployment
	render(t, "templates/operator/deployment.yaml", &op, on...)
	opEnv := envOf(op.Spec.Template.Spec.Containers[0])
	for _, k := range []string{"STATE_REGISTRY_USER", "STATE_REGISTRY_PASSWORD"} {
		if opEnv[k].ValueFrom == nil || opEnv[k].ValueFrom.SecretKeyRef.Name != "laboratory-snapshots-registry" {
			t.Errorf("operator %s must come from the registry Secret", k)
		}
	}

	ds := nodeAgent(t, on...)
	agent := ds.Spec.Template.Spec.Containers[0]
	env := envOf(agent)
	if env["STATE_REGISTRY_ADDR"].Value != "laboratory-snapshots.laboratory-system.svc:5000" || env["STATE_FORWARD_PORT"].Value != "5035" ||
		env["CGROUP_ROOT"].Value != "/host/sys/fs/cgroup" {
		t.Errorf("node-agent env %+v", env)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range agent.VolumeMounts {
		mounts[m.Name] = m
	}
	if m := mounts["containerd-root"]; m.MountPath != "/var/lib/k0s/containerd" || !m.ReadOnly {
		t.Errorf("containerd root must be mounted read-only at its host path, got %+v", m)
	}
	if mounts["host-cgroup"].MountPath != "/host/sys/fs/cgroup" {
		t.Errorf("host cgroup tree not mounted")
	}
	caps := agent.SecurityContext.Capabilities.Add
	if !hasCap(caps, "DAC_READ_SEARCH") || !hasCap(caps, "SYS_ADMIN") {
		t.Errorf("node-agent capabilities %v", caps)
	}
}

func hasCap(caps []corev1.Capability, name string) bool {
	for _, c := range caps {
		if string(c) == name {
			return true
		}
	}
	return false
}

func TestStatePersistenceValuesAreConfigurable(t *testing.T) {
	extra := []string{
		"--set", statePath + "enabled=true",
		"--set", statePath + "registry.storageClass=fast",
		"--set", statePath + "registry.size=100Gi",
		"--set", statePath + "registry.image.tag=v9.9.9",
		"--set", statePath + "debounce=12s",
		"--set", statePath + "excludePaths={/cache,/var/log}",
		"--set", statePath + "maxSnapshotSize=1Gi",
		"--set", statePath + "maxLayers=4",
		"--set", statePath + "retention=24h",
		"--set", statePath + "forwardPort=5099",
		"--set", statePath + "containerdRoot=/var/lib/containerd",
	}
	var pvc corev1.PersistentVolumeClaim
	render(t, "templates/snapshots/pvc.yaml", &pvc, extra...)
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" || pvc.Spec.Resources.Requests.Storage().String() != "100Gi" {
		t.Errorf("pvc %+v", pvc.Spec)
	}
	var dep appsv1.Deployment
	render(t, "templates/snapshots/deployment.yaml", &dep, extra...)
	if !strings.HasSuffix(dep.Spec.Template.Spec.Containers[0].Image, ":v9.9.9") {
		t.Errorf("image tag not applied: %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
	var cm corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &cm, extra...)
	want := map[string]string{
		"STATE_DEBOUNCE": "12s", "STATE_EXCLUDE_PATHS": "/cache,/var/log", "STATE_MAX_SNAPSHOT_SIZE": "1Gi",
		"STATE_MAX_LAYERS": "4", "STATE_RETENTION": "24h",
	}
	for k, v := range want {
		if cm.Data[k] != v {
			t.Errorf("%s = %q, want %q", k, cm.Data[k], v)
		}
	}
	agent := nodeAgent(t, extra...).Spec.Template.Spec.Containers[0]
	if envOf(agent)["STATE_FORWARD_PORT"].Value != "5099" {
		t.Errorf("forward port not applied")
	}
	for _, m := range agent.VolumeMounts {
		if m.Name == "containerd-root" && m.MountPath != "/var/lib/containerd" {
			t.Errorf("containerd root mount %s", m.MountPath)
		}
	}
}

func TestStatePersistenceNeedsNodeAgent(t *testing.T) {
	out, err := helmTemplate(t, "--set", statePath+"enabled=true", "--set", "nodeAgent.enabled=false")
	if err == nil {
		t.Fatalf("the chart must refuse state persistence without the node-agent")
	}
	if !strings.Contains(out, "nodeAgent.enabled") {
		t.Errorf("unexpected error: %s", out)
	}
}
