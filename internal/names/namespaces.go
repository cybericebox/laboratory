package names

// SystemNamespace is the Kubernetes namespace where the operator runs.
const SystemNamespace = "laboratory-system"

// ProxyNamespace is the Kubernetes namespace where the proxy pod runs.
const ProxyNamespace = "laboratory-proxy"

// LabGroupAccessPolicyName is the single complete firewall-policy object in
// every LabGroup namespace.
const LabGroupAccessPolicyName = "access-policy"

// AgentNamespace is the Kubernetes namespace where the management agent runs; AgentServiceAccount is its ServiceAccount (the
// operator binds it in every LabGroup namespace). The chart refuses any other agent namespace.
const (
	AgentNamespace      = "laboratory-agent"
	AgentServiceAccount = "laboratory-agent"
)

// OperatorServiceAccount is the operator's own ServiceAccount, in SystemNamespace: in every LabGroup namespace the operator binds it
// to the ClusterRole of its working permissions (it has no cluster-wide write access).
const OperatorServiceAccount = "laboratory-controller-manager"
