package names

// SystemNamespace is the Kubernetes namespace where the operator runs.
const SystemNamespace = "laboratory-system"

// ProxyNamespace is the Kubernetes namespace where the proxy pod runs.
const ProxyNamespace = "laboratory-proxy"

// WGNamespace is the Kubernetes namespace where the WireGuard demux runs, apart from the L7 proxy: its privileges depend on the demux mode.
const WGNamespace = "laboratory-wg"

// HostNetworkDemux reports whether the WireGuard demux of that mode (tc or xdp) runs on the host network, so that its packets carry a node address.
func HostNetworkDemux(mode string) bool { return mode == "tc" || mode == "xdp" }

// LabGroupAccessPolicyName is the single complete firewall-policy object in
// every LabGroup namespace.
const LabGroupAccessPolicyName = "access-policy"

// AgentNamespace is the Kubernetes namespace where the management agent runs; AgentServiceAccount is its ServiceAccount (the
// operator binds it in every LabGroup namespace). They are the defaults of the chart (agent.namespace); the operator and the agent take
// other values from AGENT_SERVICE_NAMESPACE, AGENT_SERVICE_ACCOUNT, OPERATOR_NAMESPACE, OPERATOR_SERVICE_ACCOUNT.
const (
	AgentNamespace      = "laboratory-agent"
	AgentServiceAccount = "laboratory-agent"
)

// OperatorServiceAccount is the operator's own ServiceAccount, in SystemNamespace: in every LabGroup namespace the operator binds it
// to the ClusterRole of its working permissions (it has no cluster-wide write access).
const OperatorServiceAccount = "laboratory-controller-manager"
