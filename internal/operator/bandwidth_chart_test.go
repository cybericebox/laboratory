package operator

import (
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

// renderAll renders the chart and returns every object by "Kind/name" (the namespace is in the object).
func renderAll(t *testing.T, extra ...string) map[string]map[string]any {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm not installed")
	}
	chart, err := filepath.Abs("../../charts/laboratory")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"template", "bandwidth-contract", chart, "--namespace", "laboratory-system", "--kube-version", "1.37.0", "--set", "operator.publicVPNEndpoint=vpn.example.test", "--set", "operator.baseDomain=labs.example.test", "--set", "operator.supportEmail=help@example.test"}
	data, err := exec.Command(helm, append(args, extra...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v %s", err, data)
	}
	out := map[string]map[string]any{}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil || obj["kind"] == nil {
			continue
		}
		meta, _ := obj["metadata"].(map[string]any)
		key := obj["kind"].(string) + "/" + meta["name"].(string)
		if ns, ok := meta["namespace"].(string); ok && (obj["kind"] == "Role" || obj["kind"] == "RoleBinding") {
			key += "@" + ns
		}
		out[key] = obj
	}
	return out
}

func get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// containers of a workload's pod template by name.
func podSpec(obj map[string]any) map[string]any {
	spec, _ := get(obj, "spec", "template", "spec").(map[string]any)
	return spec
}

func envOf(spec map[string]any, container string) map[string]string {
	out := map[string]string{}
	cs, _ := spec["containers"].([]any)
	for _, c := range cs {
		cm := c.(map[string]any)
		if cm["name"] != container {
			continue
		}
		envs, _ := cm["env"].([]any)
		for _, e := range envs {
			em := e.(map[string]any)
			if v, ok := em["value"].(string); ok {
				out[em["name"].(string)] = v
			}
		}
	}
	return out
}

// hasEnv says the container has an environment variable of that name (from a value or a field).
func hasEnv(spec map[string]any, container, name string) bool {
	cs, _ := spec["containers"].([]any)
	for _, c := range cs {
		cm := c.(map[string]any)
		if cm["name"] != container {
			continue
		}
		envs, _ := cm["env"].([]any)
		for _, e := range envs {
			if e.(map[string]any)["name"] == name {
				return true
			}
		}
	}
	return false
}

func volumeNames(spec map[string]any) []string {
	var out []string
	vs, _ := spec["volumes"].([]any)
	for _, v := range vs {
		out = append(out, v.(map[string]any)["name"].(string))
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestChartBandwidthOffChangesNoPod(t *testing.T) {
	objs := renderAll(t)
	if objs["ConfigMap/laboratory-config"]["data"].(map[string]any)["BANDWIDTH_ENABLED"] != "false" {
		t.Fatal("the operator is told the protection is off")
	}
	for _, k := range []string{"DaemonSet/laboratory-proxy-wg", "DaemonSet/laboratory-proxy", "Deployment/laboratory-proxy"} {
		if o, ok := objs[k]; ok && has(volumeNames(podSpec(o)), "bandwidth") {
			t.Errorf("%s gets the bandwidth volume with the protection off: a pod would restart for nothing", k)
		}
	}
	for k, o := range objs {
		if strings.HasPrefix(k, "Role/laboratory-operator-bandwidth") {
			t.Errorf("%s exists with the protection off", k)
		}
		if ann, _ := get(o, "spec", "template", "metadata", "annotations").(map[string]any); ann["cybericebox.com/bandwidth-reserve"] != nil {
			t.Errorf("%s carries a reserve annotation with the protection off: its pods would restart on upgrade", k)
		}
	}
	if env := envOf(podSpec(objs["DaemonSet/laboratory-node-agent"]), "node-agent"); env["BANDWIDTH_ENABLED"] != "" {
		t.Errorf("the node-agent is given bandwidth settings with the protection off: %v", env)
	}
}

func TestChartBandwidthOn(t *testing.T) {
	objs := renderAll(t, "--set", "bandwidth.enabled=true", "--set", "bandwidth.caps.participantMbit=30")
	cm := objs["ConfigMap/laboratory-config"]["data"].(map[string]any)
	if cm["BANDWIDTH_ENABLED"] != "true" || cm["BANDWIDTH_PARTICIPANT_CAP_MBIT"] != "30" || cm["BANDWIDTH_ENTRY_NAMESPACES"] != "laboratory-proxy,laboratory-wg" {
		t.Fatalf("operator settings: %v", cm)
	}
	env := envOf(podSpec(objs["DaemonSet/laboratory-node-agent"]), "node-agent")
	if env["BANDWIDTH_ENABLED"] != "true" || env["BANDWIDTH_GENEVE_SHARE"] != "25" || env["BANDWIDTH_LAB_GENEVE_CAP_MBIT"] != "100" {
		t.Fatalf("node-agent settings: %v", env)
	}
	wg := podSpec(objs["DaemonSet/laboratory-proxy-wg"])
	if !has(volumeNames(wg), "bandwidth") {
		t.Fatalf("the demux reads its limits from the ConfigMap volume: %v", volumeNames(wg))
	}
	var l7 map[string]any
	for _, k := range []string{"DaemonSet/laboratory-proxy", "Deployment/laboratory-proxy"} {
		if o, ok := objs[k]; ok {
			l7 = podSpec(o)
		}
	}
	if l7 == nil || !has(volumeNames(l7), "bandwidth") || !hasEnv(l7, "l7", "NODE_NAME") {
		t.Fatalf("the L7 proxy reads its limits from the ConfigMap volume")
	}
	for _, ns := range []string{"laboratory-proxy", "laboratory-wg"} {
		if objs["Role/laboratory-operator-bandwidth@"+ns] == nil || objs["RoleBinding/laboratory-operator-bandwidth@"+ns] == nil {
			t.Errorf("the operator has no right to write the ConfigMap in %s", ns)
		}
	}
	reg := objs["Deployment/laboratory-registry"]
	ann, _ := get(reg, "spec", "template", "metadata", "annotations").(map[string]any)
	if ann["cybericebox.com/bandwidth-reserve"] != "100" {
		t.Errorf("the registry declares its reserve: %v", ann)
	}
}

func TestChartBandwidthWithoutTheDemuxHasOneNamespace(t *testing.T) {
	objs := renderAll(t, "--set", "bandwidth.enabled=true", "--set", "proxy.wg.enabled=false")
	if got := objs["ConfigMap/laboratory-config"]["data"].(map[string]any)["BANDWIDTH_ENTRY_NAMESPACES"]; got != "laboratory-proxy" {
		t.Fatalf("namespaces: %v", got)
	}
}
