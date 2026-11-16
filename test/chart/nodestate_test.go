package chart_test

import (
	"strings"
	"testing"
)

func renderedNodeState(t *testing.T, extra ...string) string {
	t.Helper()
	var all strings.Builder
	// helm refuses -s of a template that renders nothing, so the whole chart is rendered and searched
	out, err := helmTemplate(t, extra...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, doc := range strings.Split(out, "\n---\n") {
		if strings.Contains(doc, "templates/node-state/") {
			all.WriteString(doc)
			all.WriteString("\n---\n")
		}
	}
	return all.String()
}

// node-state is rendered only while one of the two features that need it is on.
func TestNodeStateIsRenderedOnlyWithAFlag(t *testing.T) {
	if got := renderedNodeState(t); got != "" {
		t.Errorf("with both flags off nothing of node-state is rendered, got:\n%s", got)
	}
	for _, tc := range []struct {
		name string
		set  []string
		want bool
	}{
		{"persistence", []string{"--set", "devices.statePersistence.enabled=true"}, true},
		{"observation", []string{"--set", "lifecycle.runtimeObservation.enabled=true"}, true},
		{"profile full", []string{"--set", "lifecycle.profile=full"}, true},
		{"node-agent off", []string{"--set", "nodeAgent.enabled=false", "--set", "devices.statePersistence.enabled=true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := helmTemplate(t, tc.set...)
			if !tc.want {
				// no node-agent: the chart refuses (registry needs it); nothing of node-state either way
				if err == nil && strings.Contains(out, "laboratory-node-state") {
					t.Errorf("node-state without the node-agent must not render")
				}
				return
			}
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, out)
			}
			for _, want := range []string{"name: laboratory-node-state\n", "kind: DaemonSet", "kind: ClusterRole"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q", want)
				}
			}
		})
	}
}

// Observation alone runs the throttle monitor only: no snapshot privileges, no persistence wiring, no secrets.
func TestNodeStateWithObservationOnlyHasNoSnapshotPrivileges(t *testing.T) {
	ds := nodeState(t, "--set", "lifecycle.runtimeObservation.enabled=true")
	c := ds.Spec.Template.Spec.Containers[0]
	env := envOf(c)
	for _, k := range []string{"STATE_PERSISTENCE_ENABLED", "CONTAINERD_ROOT", "STATE_REGISTRY_USER", "STATE_REGISTRY_PASSWORD"} {
		if _, set := env[k]; set {
			t.Errorf("%s is set without state persistence", k)
		}
	}
	for _, m := range c.VolumeMounts {
		if m.Name == "containerd-root" || m.Name == "state-work" {
			t.Errorf("mount %s belongs to the snapshot engine", m.Name)
		}
	}
	if hasCap(c.SecurityContext.Capabilities.Add, "DAC_READ_SEARCH") || c.SecurityContext.AppArmorProfile != nil {
		t.Errorf("no snapshot privileges without state persistence: %+v", c.SecurityContext)
	}
	out, err := helmTemplate(t, "--set", "lifecycle.runtimeObservation.enabled=true", "-s", "templates/node-state/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, out) {
		if d["kind"] != "ClusterRole" {
			continue
		}
		for _, r := range rulesOf(t, d) {
			if has(r.Resources, "secrets") {
				t.Errorf("no Secret access without state persistence: %+v", r)
			}
		}
	}
}

// The shape of the pod: host network (it reaches the relay on the loopback of the node), its own probe port, the node-agent's
// placement, a lower priority than the node-agent, and a start with a command of the shared image.
func TestNodeStatePodShape(t *testing.T) {
	ds := nodeState(t, "--set", "devices.statePersistence.enabled=true", "--set", "labWorkloads.nodeSelector.pool=lab")
	spec := ds.Spec.Template.Spec
	if !spec.HostNetwork || spec.HostPID {
		t.Errorf("node-state is on the host network (the relay is on its loopback), never the host PID namespace")
	}
	if spec.ServiceAccountName != "laboratory-node-state" {
		t.Errorf("own ServiceAccount, got %q", spec.ServiceAccountName)
	}
	if spec.PriorityClassName == "system-node-critical" {
		t.Errorf("the node-agent keeps system-node-critical, node-state is lower")
	}
	if spec.NodeSelector["pool"] != "lab" {
		t.Errorf("node-state follows the lab placement: %v", spec.NodeSelector)
	}
	c := spec.Containers[0]
	if len(c.Command) != 2 || c.Command[0] != "/node" || c.Command[1] != "node-state" {
		t.Errorf("command %v", c.Command)
	}
	if got := envOf(c)["HEALTH_ADDR"].Value; got != "127.0.0.1:9441" {
		t.Errorf("HEALTH_ADDR %q", got)
	}
	agent := nodeAgent(t, "--set", "devices.statePersistence.enabled=true").Spec.Template.Spec
	if agent.PriorityClassName != "system-node-critical" {
		t.Errorf("the node-agent keeps system-node-critical, got %q", agent.PriorityClassName)
	}
	if c.Image != agent.Containers[0].Image {
		t.Errorf("one image for both: %s %s", c.Image, agent.Containers[0].Image)
	}
}

func TestNodeStateRefusesTheNodeAgentHealthPort(t *testing.T) {
	out, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "--set", "nodeState.healthPort=9440")
	if err == nil || !strings.Contains(out, "nodeState.healthPort must differ") {
		t.Errorf("both are on the host network: %v\n%s", err, out)
	}
}

// The node-agent keeps nothing of the snapshot engine: no AppArmor change, no DAC_READ_SEARCH, no containerd root, no Secret access.
func TestNodeAgentLostTheSnapshotEnginePrivileges(t *testing.T) {
	agent := nodeAgent(t, "--set", "lifecycle.profile=full").Spec.Template.Spec
	c := agent.Containers[0]
	if c.SecurityContext.AppArmorProfile != nil || hasCap(c.SecurityContext.Capabilities.Add, "DAC_READ_SEARCH") {
		t.Errorf("node-agent security context %+v", c.SecurityContext)
	}
	for _, v := range agent.Volumes {
		if v.Name == "containerd-root" {
			t.Errorf("the node-agent does not mount the containerd root")
		}
	}
	// the observer keeps what it needs, behind its flag
	var mounts []string
	for _, m := range c.VolumeMounts {
		mounts = append(mounts, m.Name)
	}
	if !strings.Contains(strings.Join(mounts, ","), "host-cgroup") || !strings.Contains(strings.Join(mounts, ","), "state-work") {
		t.Errorf("the observer's cgroup tree and journal stay with the node-agent: %v", mounts)
	}
	off := nodeAgent(t, "--set", "devices.statePersistence.enabled=true").Spec.Template.Spec.Containers[0]
	for _, m := range off.VolumeMounts {
		if m.Name == "host-cgroup" || m.Name == "state-work" {
			t.Errorf("without the observation the node-agent mounts %s for nothing", m.Name)
		}
	}
}

// Each ServiceAccount writes its own annotation: the policies name node-state's, and the node-agent's no longer lists the persistence one.
func TestNodeStatePoliciesAreItsOwn(t *testing.T) {
	out, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "-s", "templates/node-state/admission-policy.yaml", "-s", "templates/node-agent/admission-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: laboratory-node-state-annotation", "name: laboratory-node-state-capture", "system:serviceaccount:laboratory-system:laboratory-node-state"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "name: laboratory-node-agent-capture") {
		t.Errorf("the capture policy is node-state's now")
	}
	if strings.Contains(out, "cybericebox.com/state-persistence', 'cybericebox.com/runtime-observation'") {
		t.Errorf("the node-agent policy must not list the persistence annotation")
	}
}

func TestStatePersistenceNeedsTheRegistryAndTheNodeAgent(t *testing.T) {
	out, err := helmTemplate(t, "--set", "devices.statePersistence.enabled=true", "--set", "nodeAgent.enabled=false")
	if err == nil || !strings.Contains(out, "nodeAgent.enabled") {
		t.Errorf("persistence without the node-agent must be refused: %v\n%s", err, out)
	}
}
