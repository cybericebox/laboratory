package names

import "strings"

// Kubernetes label keys.
const (
	LabelLab    = "laboratory.cybericebox.com/lab"
	LabelDevice = "laboratory.cybericebox.com/device"
	LabelGroup  = "laboratory.cybericebox.com/group"

	// LabelPrefix is reserved for the platform: the management agent rejects user
	// labels that carry it.
	LabelPrefix = "laboratory.cybericebox.com/"
	// LabelDeployGroup is the deploy group of a LabGroup or Lab: a label-safe key
	// (base36 of a UUID, or a hash). Written by the management agent from the
	// deploy_group field; read by the operator's scheduler.
	LabelDeployGroup = LabelPrefix + "deploy-group"

	// TopologyKeyHostname is the well-known node label used as the topology key
	// for per-node scheduling constraints (device co-location).
	TopologyKeyHostname = "kubernetes.io/hostname"
)

// Kubernetes annotation keys.
const (
	// AnnotationDeployAfter lists, comma-separated, the deploy-group keys this
	// object waits for (see LabelDeployGroup).
	AnnotationDeployAfter = LabelPrefix + "deploy-after"

	// AnnotationSpecHash is the hash of the desired Lab spec as the management
	// agent received it; an idempotent create compares it, so API-server
	// defaulting of the stored spec does not look like a different spec.
	AnnotationSpecHash = LabelPrefix + "spec-hash"

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

// UserLabels returns the labels of an object that the management agent set on behalf of a
// caller: everything except the platform's own keys (LabelPrefix). Lab labels go to
// its Devices and from there to their pods.
func UserLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !strings.HasPrefix(k, LabelPrefix) {
			out[k] = v
		}
	}
	return out
}
