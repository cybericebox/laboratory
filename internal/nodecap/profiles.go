package nodecap

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cybericebox/laboratory/internal/names"
)

// Device profiles by name (the values of the Lab API field, laboratoryv1alpha1.DeviceProfile).
const (
	ProfileContainer = "container"
	ProfileMicroVM   = "microvm"
)

// ProfilePolicy says how device profiles map to nodes. The zero value is today's behaviour: only containers, no node label.
type ProfilePolicy struct {
	// Labels: a container device selects cybericebox.com/profile-container=true. Off by default, so clusters whose nodes carry no
	// profile labels keep scheduling as before.
	Labels bool
	// MicroVM: the microVM profile is offered. It always needs the profile label and the implementation label on the node.
	MicroVM bool
	// ImplementationLabel is the label of the implementation that runs the microVM profile (names.LabelRuntimeKataCLH by default).
	ImplementationLabel string
	// Overhead is the RuntimeClass overhead the scheduler adds to a microVM pod.
	Overhead Amount
	// ScaleUp lists the profiles the autoscaler node groups can add; nil: not known.
	ScaleUp []string
}

func (p ProfilePolicy) implementationLabel() string {
	if p.ImplementationLabel != "" {
		return p.ImplementationLabel
	}
	return names.LabelRuntimeKataCLH
}

// Supported says the profile is offered by this laboratory.
func (p ProfilePolicy) Supported(profile string) bool {
	switch profile {
	case "", ProfileContainer:
		return true
	case ProfileMicroVM:
		return p.MicroVM
	}
	return false
}

// Selector is the node selector a pod of the profile carries (nil: none).
func (p ProfilePolicy) Selector(profile string) map[string]string {
	switch profile {
	case ProfileMicroVM:
		return map[string]string{names.LabelProfileMicroVM: "true"}
	case "", ProfileContainer:
		if p.Labels {
			return map[string]string{names.LabelProfileContainer: "true"}
		}
	}
	return nil
}

// NodeRuns says a node with these labels can run a pod of the profile.
func (p ProfilePolicy) NodeRuns(labels map[string]string, profile string) bool {
	if !p.Supported(profile) {
		return false
	}
	for k, v := range p.Selector(profile) {
		if labels[k] != v {
			return false
		}
	}
	if profile == ProfileMicroVM && labels[p.implementationLabel()] != "true" {
		return false
	}
	return true
}

// ScaleUpKnown says the chart told which profiles the autoscaler can add.
func (p ProfilePolicy) ScaleUpKnown() bool { return p.ScaleUp != nil }

// CanScaleUp says a node group can add a node for the profile (false when not known).
func (p ProfilePolicy) CanScaleUp(profile string) bool {
	if profile == "" {
		profile = ProfileContainer
	}
	for _, s := range p.ScaleUp {
		if s == profile {
			return true
		}
	}
	return false
}

// OverheadOf is the pod overhead of the profile.
func (p ProfilePolicy) OverheadOf(profile string) Amount {
	if profile == ProfileMicroVM {
		return p.Overhead
	}
	return Amount{}
}

// AnyNodeRuns says one of the schedulable lab nodes can run the profile now.
func (p ProfilePolicy) AnyNodeRuns(nodes []corev1.Node, selector map[string]string, tolerations []corev1.Toleration, profile string) bool {
	for i := range nodes {
		if Schedulable(&nodes[i], selector, tolerations) && p.NodeRuns(nodes[i].Labels, profile) {
			return true
		}
	}
	return false
}

// ProfileConfig is read from the environment the chart gives the operator and the agent (the same chart values, written once).
type ProfileConfig struct {
	// ProfileLabels turns on the container profile label for container devices (profileLabels.enabled).
	ProfileLabels bool `env:"PROFILE_LABELS_ENABLED" envDefault:"false"`
	// MicroVMEnabled offers the microVM profile (microvm.enabled); MicroVMRuntimeClass is the RuntimeClass of its pods.
	MicroVMEnabled      bool   `env:"MICROVM_ENABLED" envDefault:"false"`
	MicroVMRuntimeClass string `env:"MICROVM_RUNTIME_CLASS" envDefault:"cybericebox-microvm"`
	// MicroVMImplementationLabel is the node label of the implementation that runs the profile.
	MicroVMImplementationLabel string `env:"MICROVM_IMPLEMENTATION_LABEL" envDefault:"cybericebox.com/runtime-kata-clh"`
	// MicroVMOverheadCPU and MicroVMOverheadMemory are the RuntimeClass overhead.podFixed (quantities).
	MicroVMOverheadCPU    string `env:"MICROVM_OVERHEAD_CPU" envDefault:"50m"`
	MicroVMOverheadMemory string `env:"MICROVM_OVERHEAD_MEMORY" envDefault:"120Mi"`
	// ScaleUpProfiles are the profiles the autoscaler node groups can add; empty: not known.
	ScaleUpProfiles []string `env:"SCALE_UP_PROFILES" envSeparator:","`
}

// Policy parses the configuration.
func (c ProfileConfig) Policy() (ProfilePolicy, error) {
	p := ProfilePolicy{Labels: c.ProfileLabels, MicroVM: c.MicroVMEnabled, ImplementationLabel: c.MicroVMImplementationLabel}
	for _, s := range c.ScaleUpProfiles {
		switch s {
		case ProfileContainer, ProfileMicroVM:
			p.ScaleUp = append(p.ScaleUp, s)
		case "":
		default:
			return p, fmt.Errorf("SCALE_UP_PROFILES: unknown profile %q", s)
		}
	}
	if c.MicroVMEnabled {
		for _, o := range []struct {
			env, val string
			cpu      bool
		}{{"MICROVM_OVERHEAD_CPU", c.MicroVMOverheadCPU, true}, {"MICROVM_OVERHEAD_MEMORY", c.MicroVMOverheadMemory, false}} {
			q, err := resource.ParseQuantity(o.val)
			if err != nil || q.Sign() < 0 {
				return p, fmt.Errorf("%s %q is not a quantity", o.env, o.val)
			}
			if o.cpu {
				p.Overhead.CPU = q.MilliValue()
			} else {
				p.Overhead.Memory = q.Value()
			}
		}
	}
	return p, nil
}

// Active says the policy changes anything: a profile label is in use, the microVM profile is on, or the autoscaler groups are known.
func (p ProfilePolicy) Active() bool { return p.Labels || p.MicroVM || p.ScaleUp != nil }
