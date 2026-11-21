package chart_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

type placedSpecs map[string]corev1.PodSpec

// placementSpecs renders the whole chart and returns the pod spec of every Deployment and DaemonSet by kind/name.
func placementSpecs(t *testing.T, extra ...string) placedSpecs {
	t.Helper()
	args := append([]string{"--set", "agent.enabled=true"}, extra...)
	out, err := helmTemplate(t, args...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	res := placedSpecs{}
	for _, doc := range strings.Split(out, "\n---") {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if yaml.Unmarshal([]byte(doc), &meta) != nil {
			continue
		}
		switch meta.Kind {
		case "Deployment":
			var d appsv1.Deployment
			if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
				t.Fatal(err)
			}
			res["Deployment/"+meta.Metadata.Name] = d.Spec.Template.Spec
		case "DaemonSet":
			var d appsv1.DaemonSet
			if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
				t.Fatal(err)
			}
			res["DaemonSet/"+meta.Metadata.Name] = d.Spec.Template.Spec
		}
	}
	return res
}

func hasToleration(spec corev1.PodSpec, key string) bool {
	for _, tol := range spec.Tolerations {
		if tol.Key == key {
			return true
		}
	}
	return false
}

const poolKey = "node-restriction.kubernetes.io/cib-pool"

// The pool of every pod of the chart (section 2.1 of the pools release): the lab pool holds what serves the labs, the lab-control pool the control.
var podPools = map[string]string{
	"Deployment/laboratory-controller-manager": "lab-control",
	"Deployment/laboratory-agent":              "lab-control",
	"Deployment/laboratory-registry":           "lab-control",
	"Deployment/laboratory-scheduler":          "lab-control",
	"Deployment/laboratory-proxy":              "lab",
	"DaemonSet/laboratory-proxy-wg":            "lab",
	"DaemonSet/laboratory-node-agent":          "lab",
	"DaemonSet/laboratory-node-state":          "lab",
	"DaemonSet/laboratory-device-access":       "lab",
	"DaemonSet/laboratory-checker":             "lab",
}

var everyPod = []string{"--set", "terminal.enabled=true", "--set", "checks.enabled=true", "--set", "devices.statePersistence.enabled=true"}

// The selector of a pool is a constant of the chart and only its tolerations are values: every pod is on its pool, whatever else is set.
func TestPodsRunOnTheirPools(t *testing.T) {
	poolTols := []string{
		"--set", "pools.lab.tolerations[0].key=cybericebox.com/lab", "--set", "pools.lab.tolerations[0].operator=Exists",
		"--set", "pools.labControl.tolerations[0].key=cybericebox.com/lab-control", "--set", "pools.labControl.tolerations[0].operator=Exists",
	}
	for _, mode := range []string{"replicas", "daemonset"} {
		specs := placementSpecs(t, append(append([]string{"--set", "proxy.mode=" + mode}, everyPod...), poolTols...)...)
		for name, pool := range podPools {
			if mode == "daemonset" && name == "Deployment/laboratory-proxy" {
				name = "DaemonSet/laboratory-proxy"
			}
			spec, ok := specs[name]
			if !ok {
				t.Errorf("%s (proxy %s) is not rendered, got %v", name, mode, keys(specs))
				continue
			}
			if len(spec.NodeSelector) != 1 || spec.NodeSelector[poolKey] != pool {
				t.Errorf("%s: nodeSelector %v, want the pool %s only", name, spec.NodeSelector, pool)
			}
			wantTol := "cybericebox.com/lab"
			if pool == "lab-control" {
				wantTol = "cybericebox.com/lab-control"
			}
			if !hasToleration(spec, wantTol) {
				t.Errorf("%s: tolerations %v lack the taint of the pool", name, spec.Tolerations)
			}
			other := "cybericebox.com/lab-control"
			if pool == "lab-control" {
				other = "cybericebox.com/lab"
			}
			if hasToleration(spec, other) {
				t.Errorf("%s: tolerates the taint of the other pool: %v", name, spec.Tolerations)
			}
		}
		for name := range specs {
			if _, ok := podPools[name]; !ok && (mode != "daemonset" || name != "DaemonSet/laboratory-proxy") {
				t.Errorf("%s is a pod of the chart with no pool listed in this test", name)
			}
		}
	}
}

// With nothing set no pod tolerates anything but what its kind needs: the startup taint of a new lab worker for a DaemonSet of the lab pool, and the
// taints Cilium and the kubelet put on a node that is not ready for the pods that must be there to make it ready.
func TestNothingSetRendersNoTolerationsOfAPool(t *testing.T) {
	for name, spec := range placementSpecs(t, everyPod...) {
		for _, tol := range spec.Tolerations {
			switch tol.Key {
			case "cybericebox.com/node-starting", "node.cilium.io/agent-not-ready":
			default:
				t.Errorf("%s tolerates %v with no pool toleration set", name, tol)
			}
		}
		if strings.HasPrefix(name, "Deployment/") && len(spec.Tolerations) != 0 {
			t.Errorf("%s: %v", name, spec.Tolerations)
		}
	}
}

// The selector cannot be set by a values file, wherever it was a value before, and the tolerations belong to the pool.
func TestThePoolSelectorIsNotAValue(t *testing.T) {
	for _, c := range []string{"operator", "agent", "registry", "labScheduler", "proxy", "proxy.wg", "nodeAgent", "nodeState", "terminal", "checks", "pools.lab", "pools.labControl"} {
		if out, err := helmTemplate(t, "--set-string", c+".nodeSelector.own=x"); err == nil {
			t.Errorf("%s.nodeSelector was accepted:\n%s", c, out)
		} else if !strings.Contains(out, c+".nodeSelector is not a value") {
			t.Errorf("%s.nodeSelector: unclear refusal:\n%s", c, out)
		}
	}
	for _, c := range []string{"operator", "agent", "registry", "labScheduler", "proxy", "proxy.wg"} {
		if out, err := helmTemplate(t, "--set", c+".tolerations[0].key=x", "--set", c+".tolerations[0].operator=Exists"); err == nil {
			t.Errorf("%s.tolerations was accepted:\n%s", c, out)
		}
	}
}

// The helper is one: the earlier fallback to a placement of all lab pods is gone.
func TestNoPlacementFallbackIsLeft(t *testing.T) {
	for _, dir := range []string{"../../charts/laboratory/templates", "../../charts/laboratory/values.yaml"} {
		if out := grepTree(t, dir, "laboratory\\.placement|"+"lab"+"Workloads"); out != "" {
			t.Errorf("a placement fallback is left:\n%s", out)
		}
	}
	if !strings.Contains(readChart(t, "templates/_helpers.tpl"), `define "laboratory.pool"`) {
		t.Error("the helper laboratory.pool is missing")
	}
}

func keys(m placedSpecs) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
