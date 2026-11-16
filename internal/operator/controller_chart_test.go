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
	if manager.Resources.Limits.Cpu().MilliValue() != 2000 || manager.Resources.Limits.Memory().Value() != 512<<20 {
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

func renderChart(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm not installed")
	}
	chart, err := filepath.Abs("../../charts/laboratory")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"template", "workers", chart, "--namespace", "laboratory-system", "--kube-version", "1.37.0", "--set", "operator.publicVPNEndpoint=vpn.example.test", "--set", "operator.baseDomain=labs.example.test", "--set", "operator.supportEmail=help@example.test"}
	data, err := exec.Command(helm, append(args, extra...)...).CombinedOutput()
	return string(data), err
}

func TestChartPassesReconcileWorkers(t *testing.T) {
	out, err := renderChart(t)
	if err != nil {
		t.Fatal(out)
	}
	for _, want := range []string{`LAB_RECONCILE_WORKERS: "8"`, `DEVICE_RECONCILE_WORKERS: "16"`, `CONNECTION_RECONCILE_WORKERS: "8"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("%s missing from the operator settings", want)
		}
	}
	for _, want := range []string{`API_QPS: "200"`, `API_BURST: "400"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("%s missing from the operator settings", want)
		}
	}
	// the image cache settings are rendered only with the cache on
	if out, err = renderChart(t, "--set", "registry.cache.enabled=true"); err != nil || !strings.Contains(out, `IMAGE_CACHE_PIN_CONCURRENCY: "8"`) {
		t.Fatalf("the pin concurrency is not passed on: %v", err)
	}
	if _, err = renderChart(t, "--set", "registry.cache.pinConcurrency=0"); err == nil {
		t.Fatal("zero pin concurrency must be refused at install time")
	}
	if _, err = renderChart(t, "--set", "operator.api.burst=10"); err == nil {
		t.Fatal("a burst below the rate must be refused at install time")
	}
	out, err = renderChart(t, "--set", "operator.workers.device=4")
	if err != nil || !strings.Contains(out, `DEVICE_RECONCILE_WORKERS: "4"`) {
		t.Fatalf("the chart value is not passed on: %v", err)
	}
	if _, err = renderChart(t, "--set", "operator.workers.lab=0"); err == nil {
		t.Fatal("zero workers must be refused at install time")
	}
}

func TestChartMetricsEndpointsAreOffByDefaultAndOptIn(t *testing.T) {
	out, err := renderChart(t)
	if err != nil {
		t.Fatal(out)
	}
	for _, not := range []string{"metrics-bind-address", "prometheus.io/scrape", "METRICS_ADDR"} {
		if strings.Contains(out, not) {
			t.Fatalf("%q is rendered with the metrics off", not)
		}
	}
	out, err = renderChart(t, "--set", "agent.enabled=true",
		"--set", "operator.metrics.enabled=true", "--set", "operator.metrics.scrapeAnnotations=true",
		"--set", "nodeAgent.metrics.enabled=true", "--set", "nodeAgent.metrics.port=9500",
		"--set", "agent.metrics.enabled=true", "--set", "agent.metrics.scrapeAnnotations=true")
	if err != nil {
		t.Fatal(out)
	}
	for _, want := range []string{"--metrics-bind-address=:8080", "--metrics-secure=false", `prometheus.io/port: "8080"`, `prometheus.io/path: /metrics`,
		`name: METRICS_ADDR`, `value: ":9500"`, `name: AGENT_METRICS_ADDR`, `prometheus.io/port: "9443"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing with the metrics on", want)
		}
	}
	// the node-agent has the endpoint but, without scrapeAnnotations, no annotation
	if strings.Count(out, "prometheus.io/scrape") != 2 {
		t.Fatalf("scrape annotations on %d pods, want the operator and the agent only", strings.Count(out, "prometheus.io/scrape"))
	}
	if _, err := renderChart(t, "--set", "agent.metrics.port=70000"); err == nil {
		t.Fatal("a port out of range must be refused")
	}
}
