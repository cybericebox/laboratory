package chart_test

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func registryPod(t *testing.T, extra ...string) corev1.PodSpec {
	t.Helper()
	var dep appsv1.Deployment
	render(t, "templates/registry/deployment.yaml", &dep, append([]string{"--set", statePath + "enabled=true"}, extra...)...)
	return dep.Spec.Template.Spec
}

func containerNamed(spec corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

// Every request to the registry passes the gate: the Service's port is the gate's, and zot listens on the loopback of the pod only.
func TestRegistryGateIsTheOnlyWayToZot(t *testing.T) {
	spec := registryPod(t)
	zot, gate := containerNamed(spec, "zot"), containerNamed(spec, "gate")
	if zot == nil || gate == nil {
		t.Fatalf("the registry pod has a zot and a gate container, got %+v", spec.Containers)
	}
	if len(zot.Ports) != 0 {
		t.Errorf("zot declares no port: it is not reachable but through the gate, %+v", zot.Ports)
	}
	if zot.ReadinessProbe != nil || zot.LivenessProbe != nil {
		t.Errorf("a loopback-only zot cannot be probed by the kubelet; the gate's readiness follows it")
	}
	var svc corev1.Service
	render(t, "templates/registry/service.yaml", &svc, "--set", statePath+"enabled=true")
	if svc.Spec.Ports[0].Port != 5000 || svc.Spec.Ports[0].TargetPort.StrVal != "registry" {
		t.Fatalf("service port %+v", svc.Spec.Ports[0])
	}
	var named []string
	for _, c := range spec.Containers {
		for _, p := range c.Ports {
			if p.Name == "registry" {
				named = append(named, c.Name)
				if p.ContainerPort != 5000 {
					t.Errorf("the registry port of %s is %d", c.Name, p.ContainerPort)
				}
			}
		}
	}
	if len(named) != 1 || named[0] != "gate" {
		t.Errorf("only the gate carries the port the Service targets, got %v", named)
	}

	var cm corev1.ConfigMap
	render(t, "templates/registry/configmap.yaml", &cm, "--set", statePath+"enabled=true")
	var cfg struct {
		HTTP struct{ Address, Port string } `json:"http"`
	}
	if err := json.Unmarshal([]byte(cm.Data["config.json"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Address != "127.0.0.1" || cfg.HTTP.Port != "5001" {
		t.Errorf("zot listens on %s:%s, want 127.0.0.1:5001", cfg.HTTP.Address, cfg.HTTP.Port)
	}
	if env := envOf(*gate); env["GATE_ZOT_ADDR"].Value != cfg.HTTP.Address+":"+cfg.HTTP.Port {
		t.Errorf("the gate dials %q, zot listens on %s:%s", env["GATE_ZOT_ADDR"].Value, cfg.HTTP.Address, cfg.HTTP.Port)
	}
}

func TestRegistryGateContainer(t *testing.T) {
	spec := registryPod(t, "--set", "registry.gate.resources.limits.memory=300Mi")
	gate := containerNamed(spec, "gate")
	if len(gate.Command) != 2 || gate.Command[0] != "/node" || gate.Command[1] != "registry-gate" {
		t.Errorf("command %v", gate.Command)
	}
	if want := nodeAgent(t).Spec.Template.Spec.Containers[0].Image; gate.Image != want {
		t.Errorf("the gate runs the node image %s, got %s", want, gate.Image)
	}
	sc := gate.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("the gate is a plain user with nothing: %+v", sc)
	}
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
		t.Errorf("the pod runs as non-root")
	}
	if gate.ReadinessProbe == nil || gate.ReadinessProbe.HTTPGet.Path != "/readyz" || gate.LivenessProbe == nil || gate.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("probes %+v %+v", gate.ReadinessProbe, gate.LivenessProbe)
	}
	if got := gate.Resources.Limits.Memory().String(); got != "300Mi" {
		t.Errorf("registry.gate.resources reaches the container: %s", got)
	}
	for _, v := range gate.VolumeMounts {
		t.Errorf("the gate mounts nothing in P1 (no volume, no Secret): %+v", v)
	}
}

// The gate follows the registry: it is not rendered without one.
func TestRegistryGateIsNotRenderedWithoutTheRegistry(t *testing.T) {
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "registry-gate") && !strings.Contains(out, "laboratory-registry") {
		t.Errorf("the gate without a registry")
	}
}

// With the image cache on, the gate gets its list, its own account and key and a read-only view of the volume, and nothing else of the
// registry Secret; zot has no sync. With it off the gate is a plain pass-through.
func TestRegistryGateCacheWiring(t *testing.T) {
	on := registryPod(t, "--set", "registry.cache.enabled=true", "--set", "registry.cache.staleIfError=7m")
	gate := containerNamed(on, "gate")
	vars := envOf(*gate)
	for k, want := range map[string]string{"GATE_CACHE_ENABLED": "true", "GATE_STALE_IF_ERROR": "7m", "GATE_DECISION_MAX_AGE": "2m", "GATE_DENIED_FOR": "30s", "GATE_DISK_HIGH_WATER": "90"} {
		if vars[k].Value != want {
			t.Errorf("%s = %q, want %q", k, vars[k].Value, want)
		}
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range gate.VolumeMounts {
		mounts[m.Name] = m
	}
	for _, name := range []string{"gate-config", "gate-secret", "data"} {
		if !mounts[name].ReadOnly {
			t.Errorf("the gate mounts %s read-only: %+v", name, mounts[name])
		}
	}
	for _, v := range on.Volumes {
		switch v.Name {
		case "gate-secret":
			var keys []string
			for _, it := range v.Secret.Items {
				keys = append(keys, it.Key)
			}
			if strings.Join(keys, ",") != "gatePassword,gateKey" {
				t.Errorf("the gate reads its own account and key and nothing else of the registry Secret: %v", keys)
			}
		case "sync-credentials":
			t.Errorf("zot has no sync, so no credentials volume")
		}
	}
	zot := containerNamed(on, "zot")
	for _, m := range zot.VolumeMounts {
		if m.Name == "gate-secret" || m.Name == "gate-config" {
			t.Errorf("zot must not see the gate's key: %+v", m)
		}
	}

	off := registryPod(t)
	g := containerNamed(off, "gate")
	if _, set := envOf(*g)["GATE_CACHE_ENABLED"]; set || len(g.VolumeMounts) != 0 {
		t.Errorf("with the cache off the gate is a pass-through with nothing mounted: %+v", g.VolumeMounts)
	}

	var sec corev1.Secret
	render(t, "templates/registry/secret.yaml", &sec, "--set", "registry.cache.enabled=true")
	if sec.StringData["gatePassword"] == "" || sec.StringData["gateKey"] == "" || !strings.Contains(sec.StringData["htpasswd"], "gate:$2") {
		t.Errorf("the registry Secret carries the gate's account and key")
	}
}
