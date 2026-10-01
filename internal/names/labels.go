package names

import "strings"

// LabelPrefix is the prefix of every operator-internal label and annotation of
// this platform. Labels with it are never copied from a Lab or LabGroup onto its
// devices and pods: they are not user labels.
const LabelPrefix = "laboratory.cybericebox.com/"

// Kubernetes label keys.
const (
	LabelLab    = "laboratory.cybericebox.com/lab"
	LabelDevice = "laboratory.cybericebox.com/device"
	LabelGroup  = "laboratory.cybericebox.com/group"
	// LabelDeployGroup is the scheduler group of a Lab or LabGroup (a key of at
	// most 63 characters). Objects with the same key are dispatched together.
	// Operator-internal: the scheduler reads it and no user label.
	LabelDeployGroup = LabelPrefix + "deploy-group"

	// TopologyKeyHostname is the well-known node label used as the topology key
	// for per-node scheduling constraints (device co-location).
	TopologyKeyHostname = "kubernetes.io/hostname"
)

// Kubernetes annotation keys.
const (
	// AnnotationDeployAfter lists, comma separated, the deploy groups that must be
	// complete (every pod Ready or failed) before the group of this object starts.
	AnnotationDeployAfter = LabelPrefix + "deploy-after"

	// AnnotationSpecHash is written by the management agent on the objects it
	// creates; the operator ignores it.
	AnnotationSpecHash = LabelPrefix + "spec-hash"

	// AnnotationUserLabels lists the user labels copied onto an object from its
	// Lab or LabGroup, so a label removed there is removed here too.
	AnnotationUserLabels = LabelPrefix + "user-labels"

	// AnnotationNetworks is the pod annotation listing OVS network attachments.
	// Format: comma-separated "iface@name[|MAC]" entries.
	AnnotationNetworks = "network.cybericebox.com/networks"

	// AnnotationDefaultNetwork controls how cni-gate handles the default k8s network.
	// Missing: regular pod — delegate eth0 to k8s CNI as normal.
	// Empty "": no default network — cni-gate returns stub eth0 only.
	// Non-empty: interface name to wire via k8s CNI (e.g. "accessport").
	AnnotationDefaultNetwork = "network.cybericebox.com/default-network"

	// AnnotationDevice tags a pod/resource with its logical device name.
	AnnotationDevice = "cybericebox.com/device"

	// DefaultEth0 is the standard container ethernet interface name.
	DefaultEth0 = "eth0"

	// AccessPortIface is the reserved in-pod interface name that carries the
	// delegated Kubernetes network for web-exposed device pods. Lab interface
	// names must not collide with it: SetupNetworks would delete/replace the
	// delegated interface on a kubelet CNI retry.
	AccessPortIface = "accessport"
)

// securityPresetCaps maps a device SecurityPreset name to the concrete Linux
// capabilities it grants. This mapping is internal — the public spec exposes
// only the preset name — so the capability requirement surface stays hidden and
// can be re-homed behind a custom agent later. Lab isolation is enforced by
// host-side OVS flows, so NET_ADMIN inside a pod cannot break out of its VNI.
var securityPresetCaps = map[string][]string{
	"":        nil, // unset == basic
	"basic":   nil,
	"service": {"NET_BIND_SERVICE"},
	"net":     {"NET_ADMIN", "NET_RAW", "NET_BIND_SERVICE", "NET_BROADCAST"},
	"debug":   {"NET_ADMIN", "NET_RAW", "NET_BIND_SERVICE", "NET_BROADCAST", "SYS_PTRACE", "SYS_NICE", "IPC_LOCK"},
}

// CapabilitiesForPreset returns the capability list for a preset name (unknown
// presets resolve to basic/none).
func CapabilitiesForPreset(preset string) []string {
	return securityPresetCaps[preset]
}

// DHCPImpliedCapabilities are added to any device with an in-image DHCP
// interface (addr.type=dhcp) so its client can send raw broadcast DISCOVERs and
// set the lease, regardless of the chosen preset.
var DHCPImpliedCapabilities = []string{"NET_ADMIN", "NET_RAW"}

// Component names used for Deployment names, Service names, and app label values.
const ComponentGateway = "gateway"

// ProxyL7App is the `app` label value on the L7 proxy pod that terminates
// external HTTPS and connects to exposed device Services. The web-exposure
// NetworkPolicy must admit this exact value (the proxy split renamed it away
// from the old "proxy").
const ProxyL7App = "laboratory-proxy-l7"

// RBAC resource names created per LabGroup namespace.
const (
	RoleVPNName     = "laboratory-vpn-role"
	RoleGatewayName = "laboratory-gateway-role"
	RoleAgentName   = "laboratory-agent-role"

	// AgentRoleBindingName is the RoleBinding created in each LabGroup namespace
	// that grants the management-agent ServiceAccount access via RoleAgentName,
	// gated by LabGroupReconciler.AgentEnabled.
	AgentRoleBindingName = "laboratory-agent-binding"
)

// Secret name patterns.
const (
	SecretVPNKeypair   = "vpn-server-keypair"
	SecretClientPrefix = "client-"
)

// IsReservedLabel reports whether a label key belongs to the platform or the system: the
// platform prefix, Kubernetes' own domains (kubernetes.io, k8s.io and their subdomains), and
// the keys the operator sets on workloads (app, pod-template-hash). Such keys are never
// exposed, searched or set through the management agent.
func IsReservedLabel(key string) bool {
	if strings.HasPrefix(key, LabelPrefix) || key == "app" || key == "pod-template-hash" || key == "controller-revision-hash" {
		return true
	}
	domain, _, ok := strings.Cut(key, "/")
	if !ok {
		return false
	}
	for _, d := range []string{"kubernetes.io", "k8s.io"} {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// UserLabels returns the labels of an object that the management agent set on behalf of a
// caller: everything except the reserved keys (IsReservedLabel). Lab labels go to
// its Devices and from there to their pods.
func UserLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !IsReservedLabel(k) {
			out[k] = v
		}
	}
	return out
}
