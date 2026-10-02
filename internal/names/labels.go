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

	// LabelNodeAgentReady is set to "true" on a node by the node-agent running there once it can serve pods, and removed when it stops.
	// Every lab pod requires it (through the lab node selector of the chart), so no lab pod lands on a node without a working
	// node-agent (a pod there would have no cni-gate and no OVS wiring).
	LabelNodeAgentReady = LabelPrefix + "node-agent-ready"

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

	// AnnotationNetworks is the pod annotation listing the OVS network attachments of a DEVICE pod. The VPN and gateway pods of a group have none:
	// the node-agent derives their lab interfaces from the group's LabVPN and LabGateway objects.
	// Format: comma-separated "iface@name[|MAC]" entries.
	AnnotationNetworks = "network.cybericebox.com/networks"

	// AnnotationDefaultNetwork controls how cni-gate handles the default k8s network.
	// Missing: regular pod — delegate eth0 to k8s CNI as normal.
	// Empty "": no default network — cni-gate returns stub eth0 only.
	// Non-empty: interface name to wire via k8s CNI (e.g. "accessport").
	AnnotationDefaultNetwork = "network.cybericebox.com/default-network"

	// AnnotationConntrackAccounting on a pod asks the node-agent to switch on conntrack byte accounting and flow
	// timestamps in the pod's network namespace when the pod is wired (CNI ADD). The pod cannot do it itself without
	// being privileged (/proc/sys is read-only for an unprivileged container).
	AnnotationConntrackAccounting = "network.cybericebox.com/conntrack-accounting"

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

// DHCPImpliedCapabilities are added to any device with an in-image DHCP
// interface (addr.type=dhcp) so its client can send raw broadcast DISCOVERs and
// set the lease, regardless of the chosen preset.
var DHCPImpliedCapabilities = []string{"NET_ADMIN", "NET_RAW"}

// Component names used for Deployment names, Service names, and app label values.
const (
	ComponentGateway = "gateway"
	ComponentVPN     = "vpn"
)

// LabelComponent marks the operator's own pods of a group namespace (the VPN and the gateway). Every selector of a
// system pod (Services, network policies, label sync, scheduler, node-agent) uses it and never the `app` label: `app` is
// also set on device pods from the device name, and the platform prefix keeps a tenant from setting this one.
const LabelComponent = LabelPrefix + "component"

// ReservedDeviceNames are the names of the endpoints the platform provides in every lab (the VPN, the internet gateway)
// and of its pods; a device may not have them.
var ReservedDeviceNames = []string{"vpn", "gateway", "internet"}

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
	// RoleOperatorNamespacedName is the ClusterRole with the operator's working permissions INSIDE a namespace;
	// OperatorRoleBindingName is the RoleBinding that grants it in each LabGroup namespace.
	RoleOperatorNamespacedName = "laboratory-operator-namespaced"
	OperatorRoleBindingName    = "laboratory-operator-binding"

	// RoleProxyReportsName is the ClusterRole with the one thing the L7 proxy writes in a LabGroup namespace (its LabTrafficReports);
	// ProxyReportsBindingName is the RoleBinding that grants it there. The proxy has no such right cluster-wide.
	RoleProxyReportsName    = "laboratory-proxy-reports"
	ProxyReportsBindingName = "laboratory-proxy-reports-binding"

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

// PropagatedLabels are the labels a Lab or LabGroup passes on to its Devices and pods: the
// caller's own labels and the tenant stamp.
func PropagatedLabels(in map[string]string) map[string]string {
	out := UserLabels(in)
	if t := in[LabelTenant]; t != "" {
		out[LabelTenant] = t
	}
	return out
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
