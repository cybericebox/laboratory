package finalizers

// Finalizer strings used across all CyberICEBox operator components.
// Every controller/binary that sets or checks a finalizer must use these constants.
const (
	Controller     = "cybericebox.com/controller"
	VPN            = "cybericebox.com/vpn"
	Gateway        = "cybericebox.com/gateway"
	Lab            = "cybericebox.com/lab"
	LabGroup       = "cybericebox.com/labgroup"
	LabGroupClient = "cybericebox.com/labgroupclient"
	NodeAgent      = "cybericebox.com/node-agent"
	OVSCleanup     = "cybericebox.com/ovs-cleanup"
)
