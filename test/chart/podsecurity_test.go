package chart_test

import (
	"testing"
)

// podSpecs returns the pod spec of every workload of a rendered chart, keyed by "<kind>/<name>".
func podSpecs(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	res := map[string]map[string]any{}
	for _, d := range docs(t, out) {
		kind, _ := d["kind"].(string)
		if kind != "Deployment" && kind != "DaemonSet" && kind != "StatefulSet" {
			continue
		}
		md := d["metadata"].(map[string]any)
		spec := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		res[kind+"/"+md["name"].(string)] = spec
	}
	return res
}

func containersOf(spec map[string]any, key string) []map[string]any {
	var out []map[string]any
	list, _ := spec[key].([]any)
	for _, c := range list {
		out = append(out, c.(map[string]any))
	}
	return out
}

func fullChart(t *testing.T, extra ...string) string {
	t.Helper()
	set := append(append([]string{}, agentSet...), "--set", "devices.statePersistence.enabled=true", "--set", "registry.cache.enabled=true")
	out, err := helmTemplate(t, append(set, extra...)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return out
}

// Every service pod of the laboratory, init and sidecar containers included, has requests and limits for CPU and memory.
func TestEveryServicePodHasRequestsAndLimits(t *testing.T) {
	specs := podSpecs(t, fullChart(t))
	if len(specs) < 5 {
		t.Fatalf("expected the operator, agent, proxy, registry and node-agent: %v", len(specs))
	}
	for name, spec := range specs {
		for _, key := range []string{"initContainers", "containers"} {
			for _, c := range containersOf(spec, key) {
				res, _ := c["resources"].(map[string]any)
				for _, kind := range []string{"requests", "limits"} {
					m, _ := res[kind].(map[string]any)
					for _, r := range []string{"cpu", "memory"} {
						if m[r] == nil {
							t.Errorf("%s %s %s: no %s %s", name, key, c["name"], kind, r)
						}
					}
				}
			}
		}
	}
}

func securityOf(c map[string]any) map[string]any {
	sc, _ := c["securityContext"].(map[string]any)
	return sc
}

func dropsAll(sc map[string]any) bool {
	caps, _ := sc["capabilities"].(map[string]any)
	drop, _ := caps["drop"].([]any)
	return len(drop) == 1 && drop[0] == "ALL"
}

// The agent and the proxy run non-root with every capability dropped, a read-only root and the runtime's seccomp profile;
// zot runs non-root with seccomp.
func TestServicePodsAreHardened(t *testing.T) {
	specs := podSpecs(t, fullChart(t))
	podLevel := func(name string) map[string]any {
		spec, ok := specs[name]
		if !ok {
			t.Fatalf("no %s in %v", name, specs)
		}
		sc, _ := spec["securityContext"].(map[string]any)
		if sc["runAsNonRoot"] != true {
			t.Errorf("%s must run as non-root: %v", name, sc)
		}
		if sp, _ := sc["seccompProfile"].(map[string]any); sp["type"] != "RuntimeDefault" {
			t.Errorf("%s seccomp: %v", name, sc)
		}
		return spec
	}
	for _, name := range []string{"Deployment/laboratory-agent", "Deployment/laboratory-proxy"} {
		for _, c := range containersOf(podLevel(name), "containers") {
			sc := securityOf(c)
			if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false || !dropsAll(sc) {
				t.Errorf("%s %s: %v", name, c["name"], sc)
			}
		}
	}
	podLevel("Deployment/laboratory-registry")
	// the node-agent drives OVS and pod networking and stays privileged
	if _, ok := specs["DaemonSet/laboratory-node-agent"]; !ok {
		t.Fatal("no node-agent")
	}
}
