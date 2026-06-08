package names

// Kubernetes label keys.
const (
	LabelLab    = "laboratory.cybericebox.com/lab"
	LabelDevice = "laboratory.cybericebox.com/device"
	LabelGroup  = "laboratory.cybericebox.com/group"
)

// Kubernetes annotation keys.
const (
	// AnnotationNetworks is the pod annotation listing OVS network attachments.
	// Format: comma-separated "iface@name[|MAC]" entries.
	// Use "iface@default" to request the real Kubernetes CNI network on that interface.
	AnnotationNetworks = "network.cybericebox.com/networks"

	// AnnotationDevice tags a pod/resource with its logical device name.
	AnnotationDevice = "cybericebox.com/device"

	// DefaultNetworkValue is the magic connection name meaning "delegate to the k8s CNI plugin".
	DefaultNetworkValue = "default"
)

// Component names used for Deployment names, Service names, and app label values.
const (
	ComponentVPN     = "vpn"
	ComponentGateway = "gateway"
)

// RBAC resource names created per LabGroup namespace.
const (
	RoleManagerName = "laboratory-manager-role"
	RoleVPNName     = "laboratory-vpn-role"
	RoleGatewayName = "laboratory-gateway-role"
)

// Secret name patterns.
const (
	SecretVPNKeypair   = "vpn-server-keypair"
	SecretClientPrefix = "client-"
)
