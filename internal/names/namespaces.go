package names

// SystemNamespace is the Kubernetes namespace where the operator runs.
const SystemNamespace = "laboratory-system"

// ProxyNamespace is the Kubernetes namespace where the proxy pod runs.
const ProxyNamespace = "laboratory-proxy"

// LabGroupAccessPolicyName is the single complete firewall-policy object in
// every LabGroup namespace.
const LabGroupAccessPolicyName = "access-policy"
