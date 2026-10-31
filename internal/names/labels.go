package names

import (
	"strings"

	pubnames "github.com/cybericebox/laboratory/pkg/names"
)

// LabelPrefix is the prefix of every operator-internal label and annotation of
// this platform. Labels with it are never copied from a Lab or LabGroup onto its
// devices and pods: they are not user labels.
const LabelPrefix = "laboratory.cybericebox.com/"

// Kubernetes label keys.
const (
	LabelLab    = "laboratory.cybericebox.com/lab"
	LabelDevice = "laboratory.cybericebox.com/device"
	LabelGroup  = "laboratory.cybericebox.com/group"
	// LabelLabUID and LabelDeviceUID mark the pod of a device with the UIDs of its Lab and its Device. The node-agent reads them
	// from the runtime to attribute containers; they replace the environment variables the containers used to carry (a participant
	// sees the environment, never the labels).
	LabelLabUID    = LabelPrefix + "lab-uid"
	LabelDeviceUID = LabelPrefix + "device-uid"
	// LabelDeployGroup is the scheduler group of a Lab or LabGroup (a key of at
	// most 63 characters). Objects with the same key are dispatched together.
	// Operator-internal: the scheduler reads it and no user label.
	LabelDeployGroup = LabelPrefix + "deploy-group"
	// LabelReservation is the name of the Reservation a Lab or LabGroup runs under. Objects without it are unreserved.
	LabelReservation = LabelPrefix + "reservation"

	// LabelNodeAgentReady is set to "true" on a node by the node-agent running there once it can serve pods, and removed when it stops.
	// Every lab pod requires it (through the lab node selector of the chart), so no lab pod lands on a node without a working
	// node-agent (a pod there would have no cni-gate and no OVS wiring).
	LabelNodeAgentReady = LabelPrefix + "node-agent-ready"

	// LabelProfileContainer and LabelProfileMicroVM are set on a node for every device profile it can run (a KVM node usually
	// carries both). LabelRuntimeKataCLH is the implementation label: the node has Kata with Cloud Hypervisor. It stays inside the
	// laboratory; the microVM RuntimeClass selects it.
	LabelProfileContainer = "cybericebox.com/profile-container"
	LabelProfileMicroVM   = "cybericebox.com/profile-microvm"
	LabelRuntimeKataCLH   = "cybericebox.com/runtime-kata-clh"

	// TopologyKeyHostname is the well-known node label used as the topology key
	// for per-node scheduling constraints (device co-location).
	TopologyKeyHostname = "kubernetes.io/hostname"
)

// Kubernetes annotation keys.
const (
	// AnnotationDeployAfter lists, comma separated, the deploy groups that must be
	// complete (every pod Ready or failed) before the group of this object starts.
	AnnotationDeployAfter = LabelPrefix + "deploy-after"

	// AnnotationDeployPriority is an explicit dispatch priority of a Lab or LabGroup (an integer, 0 when absent or not a
	// number): within the same deploy group (or among independent objects) a higher one is dispatched first, whatever the
	// size; at equal priority the larger lab goes first.
	AnnotationDeployPriority = LabelPrefix + "deploy-priority"

	// AnnotationEvictAt is set by the scheduler on every unreserved Lab and LabGroup (no reservation label) of a tenant whose
	// running work holds room that active reservations cannot get: the RFC 3339 time it is deleted at, unless the shortfall
	// clears first (then the annotation is removed).
	AnnotationEvictAt = LabelPrefix + "evict-at"

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

	// AnnotationSafeToEvict "false" on a lab pod tells a cluster autoscaler never to evict it to remove its node: a running lab
	// is never moved (the lab-group PodDisruptionBudget says the same for a drain).
	AnnotationSafeToEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"

	// AnnotationDevice tags a pod/resource with its logical device name.
	AnnotationDevice = "cybericebox.com/device"

	// LabelNodeBandwidth is the label of a node with the bandwidth of its network in Mbit/s ("1562"; a trailing M or G is
	// accepted). A node without it is taken as the chart default. AWS sets it from the launch template of the node group, Proxmox
	// from the inventory.
	LabelNodeBandwidth = "cybericebox.com/node-bandwidth"

	// AnnotationBandwidthReserve on a platform pod is the bandwidth in Mbit/s the pod keeps for itself; it is taken off the bandwidth
	// of the node the pod runs on, and of no other node.
	AnnotationBandwidthReserve = "cybericebox.com/bandwidth-reserve"

	// AnnotationBandwidthLimits is what the operator writes for the bandwidth protection: on a node the Geneve ceilings the
	// node-agent applies, on a gateway, demux or L7 proxy pod the class and per-participant (per-lab) ceilings the pod applies
	// itself. See package bandwidth for the format.
	AnnotationBandwidthLimits = "cybericebox.com/bandwidth-limits"

	// DefaultEth0 is the standard container ethernet interface name.
	DefaultEth0 = "eth0"

	// AccessPortIface is the reserved in-pod interface name that carries the
	// delegated Kubernetes network for web-exposed device pods. Lab interface
	// names must not collide with it: SetupNetworks would delete/replace the
	// delegated interface on a kubelet CNI retry.
	AccessPortIface = "accessport"

	// AccessPortRouteTable is the routing table that holds every route of the access port inside the pod. The rule
	// "from <access port address> lookup <table>" sends the replies back out of the port; the main table, where the lab
	// interfaces and their default gateway live, never holds a route through it (see package accessroute).
	AccessPortRouteTable = 100
	// AccessPortRulePriority is the priority of that rule: above the main table (32766), so the replies never reach
	// the lab's default route.
	AccessPortRulePriority = 1000
)

// The readiness probe of a device (see package internal/probe). A static program is copied by an init container into an
// emptyDir that the device container mounts read-only, because a device image may carry no shell.
const (
	// ProbeInitContainer installs the probe; ProbeVolume is the emptyDir it writes to.
	ProbeInitContainer = "cib-probe"
	ProbeVolume        = "cib-probe"
	// ProbeDir is where the device container sees the probe; ProbeBinary is the program in it.
	ProbeDir    = "/.cib/probe"
	ProbeBinary = ProbeDir + "/probe"
	// ProbeImageBinary is the probe in the node image, the image of the installing init container.
	ProbeImageBinary = "/probe"
	// ReadyFile is the marker an application creates when it is ready (WaitReadyFile).
	ReadyFile = "/run/ready"
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

// ReservedDeviceNames are the names of the endpoints the platform provides in every lab; the list is public (pkg/names) so that callers share it.
var ReservedDeviceNames = pubnames.ReservedDeviceNames

// ProxyL7App is the `app` label value on the L7 proxy pod that terminates
// external HTTPS and connects to exposed device Services. The web-exposure
// NetworkPolicy must admit this exact value (the proxy split renamed it away
// from the old "proxy").
const ProxyL7App = "laboratory-proxy-l7"

// WGDemuxApp is the `app` label value on the WireGuard demux pods (namespace WGNamespace); the VPN pods admit WireGuard from them.
const WGDemuxApp = "laboratory-proxy-wg"

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
