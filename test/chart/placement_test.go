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

var labPlacement = []string{
	"--set-string", "labWorkloads.nodeSelector.cybericebox\\.com/lab=true",
	"--set", "labWorkloads.tolerations[0].key=cybericebox.com/lab",
	"--set", "labWorkloads.tolerations[0].operator=Equal",
	"--set-string", "labWorkloads.tolerations[0].value=true",
	"--set", "labWorkloads.tolerations[0].effect=NoSchedule",
}

// Every component of the chart follows the lab-node placement unless it has its own values; with nothing set nothing renders.
func TestAllComponentsFollowLabNodes(t *testing.T) {
	t.Run("labWorkloads reaches every pod", func(t *testing.T) {
		specs := placementSpecs(t, append([]string{"--set", "proxy.mode=replicas"}, labPlacement...)...)
		for _, want := range []string{"Deployment/laboratory-controller-manager", "Deployment/laboratory-proxy", "DaemonSet/laboratory-node-agent"} {
			if _, ok := specs[want]; !ok {
				t.Fatalf("%s not rendered, got %v", want, keys(specs))
			}
		}
		if len(specs) < 6 {
			t.Fatalf("expected the operator, agent, registry, lab-scheduler, proxy and node-agent, got %v", keys(specs))
		}
		for name, spec := range specs {
			if spec.NodeSelector["cybericebox.com/lab"] != "true" {
				t.Errorf("%s: nodeSelector %v", name, spec.NodeSelector)
			}
			if !hasToleration(spec, "cybericebox.com/lab") {
				t.Errorf("%s: tolerations %v", name, spec.Tolerations)
			}
			if _, bad := spec.NodeSelector["laboratory.cybericebox.com/node-agent-ready"]; bad {
				t.Errorf("%s must not select node-agent-ready", name)
			}
		}
	})
	t.Run("proxy daemonset follows too", func(t *testing.T) {
		specs := placementSpecs(t, append([]string{"--set", "proxy.mode=daemonset"}, labPlacement...)...)
		spec, ok := specs["DaemonSet/laboratory-proxy"]
		if !ok || spec.NodeSelector["cybericebox.com/lab"] != "true" || !hasToleration(spec, "cybericebox.com/lab") {
			t.Fatalf("proxy daemonset: %v %+v", ok, spec.NodeSelector)
		}
	})
	t.Run("own values win", func(t *testing.T) {
		own := []string{}
		for _, c := range []string{"operator", "agent", "registry", "labScheduler", "proxy", "proxy.wg"} {
			own = append(own,
				"--set-string", c+".nodeSelector.own="+c,
				"--set", c+".tolerations[0].key=own-"+c,
				"--set", c+".tolerations[0].operator=Exists")
		}
		specs := placementSpecs(t, append(own, labPlacement...)...)
		want := map[string]string{
			"Deployment/laboratory-controller-manager": "operator",
			"Deployment/laboratory-proxy":              "proxy",
			"DaemonSet/laboratory-proxy-wg":            "proxy.wg",
		}
		checked := 0
		for name, spec := range specs {
			if strings.HasPrefix(name, "DaemonSet/laboratory-node-agent") {
				if spec.NodeSelector["cybericebox.com/lab"] != "true" {
					t.Errorf("%s keeps the lab placement: %v", name, spec.NodeSelector)
				}
				continue
			}
			comp := ""
			for k, v := range spec.NodeSelector {
				if k == "own" {
					comp = v
				}
			}
			if comp == "" {
				t.Errorf("%s: own selector missing: %v", name, spec.NodeSelector)
				continue
			}
			if _, mixed := spec.NodeSelector["cybericebox.com/lab"]; mixed {
				t.Errorf("%s: own selector must replace the lab one: %v", name, spec.NodeSelector)
			}
			if !hasToleration(spec, "own-"+comp) || hasToleration(spec, "cybericebox.com/lab") {
				t.Errorf("%s: tolerations %v", name, spec.Tolerations)
			}
			if w, ok := want[name]; ok && w != comp {
				t.Errorf("%s: %s", name, comp)
			}
			checked++
		}
		if checked < 5 {
			t.Errorf("checked only %d pods of %v", checked, keys(specs))
		}
	})
	t.Run("nothing set renders nothing", func(t *testing.T) {
		for name, spec := range placementSpecs(t) {
			if strings.HasPrefix(name, "DaemonSet/laboratory-node-agent") {
				if len(spec.NodeSelector) != 0 {
					t.Errorf("%s: %v", name, spec.NodeSelector)
				}
				continue
			}
			tolerations := spec.Tolerations
			if strings.HasPrefix(name, "DaemonSet/") && len(tolerations) == 1 && tolerations[0].Key == "cybericebox.com/node-starting" {
				tolerations = nil // a DaemonSet always tolerates the startup taint of a new lab worker
			}
			if len(spec.NodeSelector) != 0 || len(tolerations) != 0 {
				t.Errorf("%s: %v %v", name, spec.NodeSelector, spec.Tolerations)
			}
		}
	})
}

func keys(m placedSpecs) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
