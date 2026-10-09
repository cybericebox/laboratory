package operator

import (
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func renderControllerDeployments(t *testing.T, extra ...string) map[string]appsv1.Deployment {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm not installed")
	}
	chart, err := filepath.Abs("../../charts/laboratory")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"template", "controller-contract", chart, "--namespace", "laboratory-system", "--kube-version", "1.37.0", "--set", "operator.publicVPNEndpoint=vpn.example.test", "--set", "operator.baseDomain=labs.example.test", "--set", "operator.supportEmail=help@example.test"}
	args = append(args, extra...)
	data, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v %s", err, data)
	}
	out := map[string]appsv1.Deployment{}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var dep appsv1.Deployment
		if err := json.Unmarshal(raw, &dep); err != nil {
			continue
		}
		if dep.Kind == "Deployment" {
			out[dep.Name] = dep
		}
	}
	return out
}

func TestControllerStartupProbeAllowsExistingBootstrapBudgets(t *testing.T) {
	deployments := renderControllerDeployments(t)
	manager := deployments["laboratory-controller-manager"].Spec.Template.Spec.Containers[0]
	if manager.StartupProbe == nil {
		t.Fatal("liveness starts before the existing60sCRD and90sadmission waits can finish")
	}
	if manager.StartupProbe.HTTPGet == nil || manager.StartupProbe.HTTPGet.Path != "/healthz" || manager.StartupProbe.HTTPGet.Port.IntVal != 8081 {
		t.Fatal("startup must use the liveness health endpoint")
	}
	if budget := manager.StartupProbe.PeriodSeconds * manager.StartupProbe.FailureThreshold; budget < 150 {
		t.Fatalf("startup budget%d cuts existing bootstrap waits", budget)
	}
	if manager.LivenessProbe.PeriodSeconds != 20 || manager.ReadinessProbe.PeriodSeconds != 10 || manager.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatal("normal probe behavior changed")
	}
	if manager.Resources.Limits.Cpu().MilliValue() != 500 || manager.Resources.Limits.Memory().Value() != 128<<20 {
		t.Fatal("controller resource limits changed")
	}
	scheduler := deployments["laboratory-scheduler"].Spec.Template.Spec.Containers[0]
	if scheduler.Command[0] != "kube-scheduler" || scheduler.Resources.Limits.Memory().Value() != 256<<20 {
		t.Fatal("external scheduler integration changed")
	}
}

func TestControllerChartKeepsDisabledSchedulerAndLeaderVariant(t *testing.T) {
	deployments := renderControllerDeployments(t, "--set", "labScheduler.enabled=false", "--set", "operator.leaderElection=false")
	if _, ok := deployments["laboratory-scheduler"]; ok {
		t.Fatal("disabled external scheduler rendered")
	}
	manager := deployments["laboratory-controller-manager"].Spec.Template.Spec.Containers[0]
	if len(manager.Args) != 1 || manager.Args[0] != "--leader-elect=false" {
		t.Fatal("explicit leader-election override lost")
	}
	if manager.StartupProbe == nil || manager.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatal("variant probe contract changed")
	}
}
