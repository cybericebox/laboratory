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
	// the gate refuses a registry that is not on registry.allowedRegistries before any upstream call: the same list as the operator's
	if got := vars["GATE_ALLOWED_REGISTRIES"].Value; !strings.Contains(","+got+",", ",ghcr.io,") || strings.Contains(got, "*") {
		t.Errorf("GATE_ALLOWED_REGISTRIES = %q: want the chart's allowedRegistries", got)
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

// The credentials of a cached pull cross the cluster network between the relay of a node and the gate: that hop is TLS, the gate serves
// the cache on its TLS port only, and the node-agent trusts the authority of this registry alone.
func TestRegistryGateTLSHop(t *testing.T) {
	cache := []string{"--set", "registry.cache.enabled=true"}
	var sec corev1.Secret
	render(t, "templates/registry/tls-secret.yaml", &sec, cache...)
	for _, k := range []string{"ca.crt", "tls.crt", "tls.key"} {
		if !strings.Contains(sec.StringData[k], "-----BEGIN") {
			t.Errorf("the TLS Secret holds %s", k)
		}
	}
	var svc corev1.Service
	render(t, "templates/registry/service.yaml", &svc, cache...)
	if len(svc.Spec.Ports) != 2 || svc.Spec.Ports[1].Port != 5443 || svc.Spec.Ports[1].TargetPort.StrVal != "registry-tls" {
		t.Errorf("the Service has the TLS port: %+v", svc.Spec.Ports)
	}
	var plain corev1.Service
	render(t, "templates/registry/service.yaml", &plain, "--set", statePath+"enabled=true")
	if len(plain.Spec.Ports) != 1 {
		t.Errorf("without the cache there is no TLS port: %+v", plain.Spec.Ports)
	}

	spec := registryPod(t, cache...)
	gate := containerNamed(spec, "gate")
	if envOf(*gate)["GATE_TLS_LISTEN"].Value != ":5443" {
		t.Errorf("the gate listens for TLS")
	}
	for _, v := range spec.Volumes {
		if v.Name == "gate-tls" {
			var keys []string
			for _, it := range v.Secret.Items {
				keys = append(keys, it.Key)
			}
			if strings.Join(keys, ",") != "tls.crt,tls.key" {
				t.Errorf("the gate reads its certificate and key, not the authority: %v", keys)
			}
		}
	}

	agent := nodeAgent(t, cache...).Spec.Template.Spec
	vars := envOf(agent.Containers[0])
	if vars["STATE_REGISTRY_ADDR"].Value != "laboratory-registry.laboratory-system.svc:5443" || vars["STATE_REGISTRY_CA_FILE"].Value != "/etc/registry-ca/ca.crt" {
		t.Errorf("the relay dials the TLS port and trusts the registry's authority: %q %q", vars["STATE_REGISTRY_ADDR"].Value, vars["STATE_REGISTRY_CA_FILE"].Value)
	}
	found := false
	for _, v := range agent.Volumes {
		if v.Name == "registry-ca" {
			found = true
			if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "ca.crt" {
				t.Errorf("the node-agent mounts the authority only: %+v", v.Secret.Items)
			}
		}
	}
	if !found {
		t.Error("the node-agent mounts the authority of the registry")
	}
	// without the cache nothing changes for the relay: plain, to the Service port
	off := envOf(nodeAgent(t, "--set", statePath+"enabled=true").Spec.Template.Spec.Containers[0])
	if _, set := off["STATE_REGISTRY_CA_FILE"]; set || off["STATE_REGISTRY_ADDR"].Value != "laboratory-registry.laboratory-system.svc:5000" {
		t.Errorf("no cache, no TLS: %+v", off["STATE_REGISTRY_ADDR"])
	}
}

// The gate of the kind E2E: the chart is rendered with the exact flags of hack/registry-gate-e2e/run.sh, and the allowed list that the gate
// gets holds the two registries that the test uses and not the third one it knows (so the refusal is tested end to end).
func TestRegistryGateAllowedListWithTheE2EFlags(t *testing.T) {
	spec := registryPod(t,
		"--set", "registry.cache.enabled=true", "--set-json", `registry.cache.registries=["docker.io"]`, // docker.io: the platform images
		"--set-json", `registry.allowedRegistries=["pub","priv"]`,
		"--set", "registry.cache.extraRegistries[0].name=pub", "--set", "registry.cache.extraRegistries[0].url=http://upstream-pub.laboratory-system.svc:5000",
		"--set", "registry.cache.extraRegistries[1].name=priv", "--set", "registry.cache.extraRegistries[1].url=http://upstream-priv.laboratory-system.svc:5000",
		"--set", "registry.cache.extraRegistries[2].name=off", "--set", "registry.cache.extraRegistries[2].url=http://upstream-pub.laboratory-system.svc:5000",
	)
	got := envOf(*containerNamed(spec, "gate"))["GATE_ALLOWED_REGISTRIES"].Value
	list := "," + got + ","
	for _, want := range []string{",pub,", ",priv,"} {
		if !strings.Contains(list, want) {
			t.Errorf("GATE_ALLOWED_REGISTRIES = %q misses %s", got, want)
		}
	}
	if strings.Contains(list, ",off,") || strings.Contains(got, "*") || got == "" {
		t.Errorf("GATE_ALLOWED_REGISTRIES = %q: the gate must get a list that leaves the third registry out", got)
	}
}

// With the image cache on every image goes through it, so every allowed registry (and the registry of every platform image) must be one
// the cache serves, and "*" cannot be served: the chart refuses the install instead of leaving a registry that would be pulled directly.
func TestCacheOnRefusesRegistriesItDoesNotServe(t *testing.T) {
	cache := []string{"--set", "registry.cache.enabled=true"}
	for name, tc := range map[string]struct {
		extra []string
		fail  string
	}{
		"default list":      {nil, ""},
		"unserved registry": {[]string{"--set-json", `registry.allowedRegistries=["docker.io","gcr.io"]`}, `registry "gcr.io"`},
		"any registry":      {[]string{"--set-json", `registry.allowedRegistries=["*"]`}, `"*" cannot be used`},
		"served by extra": {[]string{"--set-json", `registry.allowedRegistries=["docker.io","reg.example.com:5000"]`,
			"--set", "registry.cache.extraRegistries[0].name=reg.example.com:5000", "--set", "registry.cache.extraRegistries[0].url=https://reg.example.com:5000"}, ""},
		"platform image off the list": {[]string{"--set", "vpn.image.repository=private.example.com/vpn"}, `registry "private.example.com"`},
	} {
		out, err := helmTemplate(t, append(append([]string{}, cache...), tc.extra...)...)
		switch {
		case tc.fail == "" && err != nil:
			t.Errorf("%s: %v\n%s", name, err, out)
		case tc.fail != "" && (err == nil || !strings.Contains(out, tc.fail)):
			t.Errorf("%s: want a refusal with %q, got %v\n%s", name, tc.fail, err, out)
		}
	}
	// with the cache off the list alone decides, "*" included
	if _, err := helmTemplate(t, "--set-json", `registry.allowedRegistries=["*"]`); err != nil {
		t.Errorf("cache off: %v", err)
	}
}
