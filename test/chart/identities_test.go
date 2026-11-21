package chart_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/registrygate"
)

// Who the gate accepts and how they show it: the audience of the tokens the chart projects is the one the gate checks, the ServiceAccounts the
// gate is told are the ones the workloads run as, and the constants the code shares with the chart (the namespaces, the image host) are what the
// chart renders.
func TestGateIdentitiesAreTheWorkloadsOfTheChart(t *testing.T) {
	out, err := helmTemplate(t, everything...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	pods := podsOf(t, out)
	workloads := map[string]string{ // gate env -> workload
		"GATE_NODE_ACCOUNT":     "DaemonSet/laboratory-system/laboratory-node-agent",
		"GATE_OPERATOR_ACCOUNT": "Deployment/laboratory-system/laboratory-controller-manager",
		"GATE_AGENT_ACCOUNT":    "Deployment/laboratory-agent/laboratory-agent",
	}
	var gate *corev1.Container
	for _, c := range pods["Deployment/laboratory-system/laboratory-registry"].Containers {
		if c.Name == "gate" {
			gate = &c
		}
	}
	if gate == nil {
		t.Fatal("no gate container")
	}
	env := envOf(*gate)
	for name, key := range workloads {
		spec, ok := pods[key]
		if !ok {
			t.Fatalf("%s not rendered", key)
		}
		ns := strings.Split(key, "/")[1]
		if want := ns + "/" + spec.ServiceAccountName; env[name].Value != want {
			t.Errorf("%s = %q, the workload %s runs as %q", name, env[name].Value, key, want)
		}
		var audience string
		for _, v := range spec.Volumes {
			if v.Projected == nil {
				continue
			}
			for _, s := range v.Projected.Sources {
				if s.ServiceAccountToken != nil {
					audience = s.ServiceAccountToken.Audience
					if s.ServiceAccountToken.ExpirationSeconds != nil && *s.ServiceAccountToken.ExpirationSeconds < 600 {
						t.Errorf("%s: a token valid for %ds is under the minimum the API server gives", key, *s.ServiceAccountToken.ExpirationSeconds)
					}
				}
			}
		}
		if audience != registrygate.TokenAudience {
			t.Errorf("%s projects a token for the audience %q, the gate checks %q", key, audience, registrygate.TokenAudience)
		}
	}
	// the constants of the code
	if v := env["GATE_OPERATOR_ACCOUNT"].Value; v != names.SystemNamespace+"/"+names.OperatorServiceAccount {
		t.Errorf("the operator is %q, names say %s/%s", v, names.SystemNamespace, names.OperatorServiceAccount)
	}
	if v := env["GATE_AGENT_ACCOUNT"].Value; v != names.AgentNamespace+"/"+names.AgentServiceAccount {
		t.Errorf("the agent is %q, names say %s/%s", v, names.AgentNamespace, names.AgentServiceAccount)
	}
	if v := env["GATE_IMAGE_HOST"].Value; v != names.RegistryNodePrefix {
		t.Errorf("GATE_IMAGE_HOST = %q, the operator writes images under %q", v, names.RegistryNodePrefix)
	}
	var upstream bool
	for _, d := range docs(t, out) {
		md, _ := d["metadata"].(map[string]any)
		if d["kind"] == "Namespace" && md["name"] == names.UpstreamNamespace {
			upstream = true
		}
	}
	if !upstream {
		t.Errorf("no namespace %s: the operator and the agent write the upstream credentials there, the gate reads them", names.UpstreamNamespace)
	}
}

// Every ConfigMap and Secret a pod of the chart mounts or reads exists in the chart, in the pod's own namespace, or is one something else makes
// (a cert-manager Certificate names its Secret) or the pod marks optional. A name a pod mounts that nothing makes is a pod stuck in
// ContainerCreating.
func TestEveryMountedObjectIsMadeByTheChart(t *testing.T) {
	out, err := helmTemplate(t, everything...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	have := map[string]bool{}     // "ConfigMap/ns/name"
	keys := map[string][]string{} // the keys of the ones the chart renders
	for _, d := range docs(t, out) {
		md, _ := d["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		name, _ := md["name"].(string)
		switch d["kind"] {
		case "ConfigMap", "Secret":
			id := d["kind"].(string) + "/" + ns + "/" + name
			have[id] = true
			for _, field := range []string{"data", "stringData", "binaryData"} {
				m, _ := d[field].(map[string]any)
				for k := range m {
					keys[id] = append(keys[id], k)
				}
			}
		case "Certificate":
			spec, _ := d["spec"].(map[string]any)
			if s, _ := spec["secretName"].(string); s != "" {
				have["Secret/"+ns+"/"+s] = true
			}
		}
	}
	// made while the release runs: by the operator, the agent, or the registry gate's own Secret hook
	runtime := map[string]bool{"ConfigMap/laboratory-device-access/laboratory-access-keys": true}
	for workload, spec := range podsOf(t, out) {
		key := workload
		ns := strings.Split(key, "/")[1]
		need := func(kind, name string, optional *bool) {
			id := kind + "/" + ns + "/" + name
			if !have[id] && !runtime[id] && (optional == nil || !*optional) {
				t.Errorf("%s mounts or reads %s that the chart does not make", key, id)
			}
		}
		hasKey := func(kind, name, key string) {
			id := kind + "/" + ns + "/" + name
			if ks, rendered := keys[id]; rendered && len(ks) > 0 {
				for _, k := range ks {
					if k == key {
						return
					}
				}
				t.Errorf("%s mounts the key %q of %s, which has %v", workload, key, id, ks)
			}
		}
		for _, v := range spec.Volumes {
			if v.ConfigMap != nil {
				need("ConfigMap", v.ConfigMap.Name, v.ConfigMap.Optional)
				for _, it := range v.ConfigMap.Items {
					hasKey("ConfigMap", v.ConfigMap.Name, it.Key)
				}
			}
			if v.Secret != nil {
				need("Secret", v.Secret.SecretName, v.Secret.Optional)
				for _, it := range v.Secret.Items {
					hasKey("Secret", v.Secret.SecretName, it.Key)
				}
			}
		}
		for _, list := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
			for _, c := range list {
				for _, ef := range c.EnvFrom {
					if ef.ConfigMapRef != nil {
						need("ConfigMap", ef.ConfigMapRef.Name, ef.ConfigMapRef.Optional)
					}
					if ef.SecretRef != nil {
						need("Secret", ef.SecretRef.Name, ef.SecretRef.Optional)
					}
				}
				for _, e := range c.Env {
					if e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil {
						need("ConfigMap", e.ValueFrom.ConfigMapKeyRef.Name, e.ValueFrom.ConfigMapKeyRef.Optional)
					}
					if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
						need("Secret", e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Optional)
					}
				}
			}
		}
	}
}
