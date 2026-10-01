package chart_test

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const (
	statePath = "devices.statePersistence."
	regPath   = "registry."
)

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
	for _, unwanted := range []string{"laboratory-registry", "STATE_REGISTRY", "STATE_PERSISTENCE", "containerd-root", "DAC_READ_SEARCH"} {
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
	render(t, "templates/registry/deployment.yaml", &dep, on...)
	zot := dep.Spec.Template.Spec.Containers[0]
	if zot.Image != "ghcr.io/project-zot/zot:v2.1.21" {
		t.Errorf("registry image %q", zot.Image)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("a ReadWriteOnce volume needs the Recreate strategy")
	}

	var pvc corev1.PersistentVolumeClaim
	render(t, "templates/registry/pvc.yaml", &pvc, on...)
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
	render(t, "templates/registry/service.yaml", &svc, on...)
	if svc.Name != "laboratory-registry" || svc.Spec.Ports[0].Port != 5000 {
		t.Errorf("service %+v", svc.Spec)
	}

	var secret corev1.Secret
	render(t, "templates/registry/secret.yaml", &secret, on...)
	if secret.StringData["username"] != "writer" || len(secret.StringData["password"]) < 32 || !strings.HasPrefix(secret.StringData["htpasswd"], "writer:$2") {
		t.Errorf("registry credentials not generated: %+v", secret.StringData)
	}

	var zotConfig corev1.ConfigMap
	render(t, "templates/registry/configmap.yaml", &zotConfig, on...)
	cfg := zotConfig.Data["config.json"]
	var zc map[string]any
	if err := json.Unmarshal([]byte(cfg), &zc); err != nil {
		t.Fatalf("zot config is not JSON: %v\n%s", err, cfg)
	}
	st := zc["storage"].(map[string]any)
	if st["gc"] != true || st["rootDirectory"] != "/var/lib/registry" || st["gcInterval"] != "1h" {
		t.Errorf("storage %v", st)
	}
	if _, has := zc["extensions"]; has {
		t.Errorf("without the image cache zot runs no sync extension")
	}
	if _, has := st["retention"]; has {
		t.Errorf("without the image cache no retention policy applies")
	}

	var opCfg corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &opCfg, on...)
	wantCfg := map[string]string{
		"STATE_PERSISTENCE_ENABLED": "true",
		"STATE_REGISTRY_ADDR":       "laboratory-registry.laboratory-system.svc:5000",
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
		if opEnv[k].ValueFrom == nil || opEnv[k].ValueFrom.SecretKeyRef.Name != "laboratory-registry" {
			t.Errorf("operator %s must come from the registry Secret", k)
		}
	}

	ds := nodeAgent(t, on...)
	agent := ds.Spec.Template.Spec.Containers[0]
	env := envOf(agent)
	if env["STATE_REGISTRY_ADDR"].Value != "laboratory-registry.laboratory-system.svc:5000" || env["STATE_FORWARD_PORT"].Value != "5035" ||
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
		"--set", regPath + "storageClass=fast",
		"--set", regPath + "size=100Gi",
		"--set", regPath + "image.tag=v9.9.9",
		"--set", statePath + "debounce=12s",
		"--set", statePath + "excludePaths={/cache,/var/log}",
		"--set", statePath + "maxSnapshotSize=1Gi",
		"--set", statePath + "maxLayers=4",
		"--set", statePath + "retention=24h",
		"--set", regPath + "forwardPort=5099",
		"--set", statePath + "containerdRoot=/var/lib/containerd",
	}
	var pvc corev1.PersistentVolumeClaim
	render(t, "templates/registry/pvc.yaml", &pvc, extra...)
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" || pvc.Spec.Resources.Requests.Storage().String() != "100Gi" {
		t.Errorf("pvc %+v", pvc.Spec)
	}
	var dep appsv1.Deployment
	render(t, "templates/registry/deployment.yaml", &dep, extra...)
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

func TestImageCacheOffByDefault(t *testing.T) {
	var cm corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &cm)
	for k := range cm.Data {
		if strings.HasPrefix(k, "IMAGE_CACHE") {
			t.Errorf("operator config has %s although the cache is off", k)
		}
	}
}

func TestImageCacheAloneDeploysRegistryWithoutStatePersistence(t *testing.T) {
	cache := []string{"--set", "registry.cache.enabled=true"}

	var zotCfg corev1.ConfigMap
	render(t, "templates/registry/configmap.yaml", &zotCfg, cache...)
	var zc struct {
		Extensions struct {
			Sync struct {
				Enable          bool   `json:"enable"`
				CredentialsFile string `json:"credentialsFile"`
				Registries      []struct {
					URLs           []string `json:"urls"`
					OnDemand       bool     `json:"onDemand"`
					PreserveDigest bool     `json:"preserveDigest"`
					Content        []struct {
						Prefix      string `json:"prefix"`
						Destination string `json:"destination"`
						StripPrefix bool   `json:"stripPrefix"`
					} `json:"content"`
				} `json:"registries"`
			} `json:"sync"`
		} `json:"extensions"`
		HTTP struct {
			Compat []string `json:"compat"`
		} `json:"http"`
		Storage struct {
			Retention struct {
				Policies []struct {
					Repositories []string `json:"repositories"`
					KeepTags     []struct {
						PulledWithin string `json:"pulledWithin"`
					} `json:"keepTags"`
				} `json:"policies"`
			} `json:"retention"`
		} `json:"storage"`
	}
	if err := json.Unmarshal([]byte(zotCfg.Data["config.json"]), &zc); err != nil {
		t.Fatal(err)
	}
	sync := zc.Extensions.Sync
	if !sync.Enable || sync.CredentialsFile != "/etc/zot-sync/credentials.json" || len(sync.Registries) != 4 {
		t.Fatalf("sync %+v", sync)
	}
	want := map[string]string{"/docker.io": "https://registry-1.docker.io", "/ghcr.io": "https://ghcr.io", "/quay.io": "https://quay.io", "/registry.k8s.io": "https://registry.k8s.io"}
	for _, r := range sync.Registries {
		c := r.Content[0]
		if !r.PreserveDigest || !r.OnDemand || c.Prefix != "**" || c.StripPrefix || want[c.Destination] != r.URLs[0] {
			t.Errorf("registry entry %+v", r)
		}
		delete(want, c.Destination)
	}
	if len(want) != 0 {
		t.Errorf("missing upstreams %v", want)
	}
	// Digest-pinned pulls of Docker-format images need the digests kept as they are upstream.
	if len(zc.HTTP.Compat) != 1 || zc.HTTP.Compat[0] != "docker2s2" {
		t.Errorf("http.compat must allow docker2s2 manifests: %v", zc.HTTP.Compat)
	}
	pol := zc.Storage.Retention.Policies[0]
	if pol.KeepTags[0].PulledWithin != "48h" || len(pol.Repositories) != 4 || pol.Repositories[0] != "docker.io/**" {
		t.Errorf("retention %+v", pol)
	}

	// The registry, but none of the state persistence wiring.
	var dep appsv1.Deployment
	render(t, "templates/registry/deployment.yaml", &dep, cache...)
	if dep.Spec.Template.Spec.Containers[0].Image != "ghcr.io/project-zot/zot:v2.1.21" {
		t.Errorf("the cache needs the full zot image, got %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
	var sec corev1.Secret
	render(t, "templates/registry/sync-secret.yaml", &sec, cache...)
	if sec.StringData["credentials.json"] != "{}" {
		t.Errorf("no pull secrets, no upstream credentials: %q", sec.StringData["credentials.json"])
	}

	var opCfg corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &opCfg, cache...)
	if opCfg.Data["IMAGE_CACHE_PIN_TTL"] != "30m" || opCfg.Data["IMAGE_CACHE_ENABLED"] != "true" || opCfg.Data["IMAGE_CACHE_PREFIX"] != "localhost:5035" ||
		opCfg.Data["IMAGE_CACHE_REGISTRIES"] != "docker.io,ghcr.io,quay.io,registry.k8s.io" {
		t.Errorf("operator config %v", opCfg.Data)
	}
	if _, on := opCfg.Data["STATE_PERSISTENCE_ENABLED"]; on {
		t.Errorf("the cache alone must not turn state persistence on")
	}

	agent := nodeAgent(t, cache...).Spec.Template.Spec.Containers[0]
	env := envOf(agent)
	if env["STATE_REGISTRY_ADDR"].Value == "" || env["STATE_FORWARD_PORT"].Value != "5035" {
		t.Errorf("the node-agent must forward the registry: %v", env)
	}
	if _, on := env["STATE_PERSISTENCE_ENABLED"]; on || hasCap(agent.SecurityContext.Capabilities.Add, "DAC_READ_SEARCH") {
		t.Errorf("the snapshot engine's wiring must stay off")
	}
	for _, m := range agent.VolumeMounts {
		if m.Name == "containerd-root" || m.Name == "host-cgroup" {
			t.Errorf("mount %s belongs to state persistence only", m.Name)
		}
	}
}

func TestImageCacheCustomRegistriesAndCredentialsSecret(t *testing.T) {
	extra := []string{
		"--set", "registry.cache.enabled=true",
		"--set", "registry.cache.registries={docker.io}",
		"--set", "registry.cache.extraRegistries[0].name=registry.example.com",
		"--set", "registry.cache.extraRegistries[0].url=https://registry.example.com",
		"--set", "registry.cache.credentialsSecret=my-sync-creds",
		"--set", "registry.cache.unusedTTL=12h",
		"--set", "registry.cache.pinTTL=5m",
	}
	var opCfg corev1.ConfigMap
	render(t, "templates/operator/configmap.yaml", &opCfg, extra...)
	if opCfg.Data["IMAGE_CACHE_REGISTRIES"] != "docker.io,registry.example.com" || opCfg.Data["IMAGE_CACHE_PIN_TTL"] != "5m" {
		t.Errorf("registries %q pin ttl %q", opCfg.Data["IMAGE_CACHE_REGISTRIES"], opCfg.Data["IMAGE_CACHE_PIN_TTL"])
	}
	var zotCfg corev1.ConfigMap
	render(t, "templates/registry/configmap.yaml", &zotCfg, extra...)
	if !strings.Contains(zotCfg.Data["config.json"], `"pulledWithin": "12h"`) || strings.Contains(zotCfg.Data["config.json"], "maxAge") {
		t.Errorf("unusedTTL must drive the retention, with no hard age cap:\n%s", zotCfg.Data["config.json"])
	}
	var dep appsv1.Deployment
	render(t, "templates/registry/deployment.yaml", &dep, extra...)
	found := false
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "sync-credentials" && v.Secret.SecretName == "my-sync-creds" {
			found = true
		}
	}
	if !found {
		t.Errorf("an own credentials Secret must be mounted")
	}
	if out, err := helmTemplate(t, append(extra, "-s", "templates/registry/sync-secret.yaml")...); err == nil && strings.Contains(out, "kind: Secret") {
		t.Errorf("no generated credentials Secret with credentialsSecret set:\n%s", out)
	}
	if out, err := helmTemplate(t, "--set", "registry.cache.enabled=true", "--set", "registry.cache.registries={nope.io}"); err == nil {
		t.Errorf("an unknown built-in registry must be refused:\n%s", out)
	}
}

func TestAgentPrewarmWiring(t *testing.T) {
	on := []string{"--set", "registry.cache.enabled=true", "--set", "agent.enabled=true", "--set", "agent.domain=a.example.com",
		"--set", "imagePullSecrets[0].name=pull-one", "--set", "registry.cache.prewarm.concurrency=7"}
	var dep appsv1.Deployment
	render(t, "templates/agent/deployment.yaml", &dep, on...)
	env := envOf(dep.Spec.Template.Spec.Containers[0])
	want := map[string]string{
		"AGENT_CACHE_ENABLED":         "true",
		"AGENT_CACHE_REGISTRY_ADDR":   "laboratory-registry.laboratory-system.svc:5000",
		"AGENT_CACHE_REGISTRIES":      "docker.io,ghcr.io,quay.io,registry.k8s.io",
		"AGENT_PULL_SECRETS":          "pull-one",
		"AGENT_PULL_SECRET_NAMESPACE": "laboratory-system",
		"AGENT_PREWARM_CONCURRENCY":   "7",
		"AGENT_PREWARM_TIMEOUT":       "10m",
	}
	for k, v := range want {
		if env[k].Value != v {
			t.Errorf("%s = %q, want %q", k, env[k].Value, v)
		}
	}
	out, err := helmTemplate(t, append(on, "-s", "templates/agent/role-pullsecrets.yaml")...)
	if err != nil || !strings.Contains(out, "pull-one") || !strings.Contains(out, "verbs: [ get ]") && !strings.Contains(out, "- get") {
		t.Errorf("the agent must be allowed to read exactly the pull secrets:\n%s %v", out, err)
	}

	// Cache off: the agent knows nothing of it and has no extra role.
	var off appsv1.Deployment
	render(t, "templates/agent/deployment.yaml", &off, "--set", "agent.enabled=true", "--set", "agent.domain=a.example.com")
	for k := range envOf(off.Spec.Template.Spec.Containers[0]) {
		if strings.HasPrefix(k, "AGENT_CACHE") || strings.HasPrefix(k, "AGENT_PREWARM") {
			t.Errorf("%s present with the cache off", k)
		}
	}
}

func TestAgentGetsTheRegistryAddressForSnapshotExport(t *testing.T) {
	agent := []string{"--set", "agent.enabled=true", "--set", "agent.domain=a.example.com"}
	for name, extra := range map[string][]string{
		"persistence": {"--set", statePath + "enabled=true"},
		"cache":       {"--set", "registry.cache.enabled=true"},
	} {
		var dep appsv1.Deployment
		render(t, "templates/agent/deployment.yaml", &dep, append(agent, extra...)...)
		if got := envOf(dep.Spec.Template.Spec.Containers[0])["AGENT_REGISTRY_ADDR"].Value; got != "laboratory-registry.laboratory-system.svc:5000" {
			t.Errorf("%s: AGENT_REGISTRY_ADDR = %q", name, got)
		}
	}
	var off appsv1.Deployment
	render(t, "templates/agent/deployment.yaml", &off, agent...)
	if _, set := envOf(off.Spec.Template.Spec.Containers[0])["AGENT_REGISTRY_ADDR"]; set {
		t.Error("no registry, no address")
	}
}
