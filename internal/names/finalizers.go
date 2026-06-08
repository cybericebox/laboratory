package names

// Finalizer strings set by each controller/binary component.
// Every controller that adds or checks a finalizer must use these constants.
const (
	FinalizerController     = "cybericebox.com/controller"
	FinalizerVPN            = "cybericebox.com/vpn"
	FinalizerGateway        = "cybericebox.com/gateway"
	FinalizerLab            = "cybericebox.com/lab"
	FinalizerLabGroup       = "cybericebox.com/labgroup"
	FinalizerLabGroupClient = "cybericebox.com/labgroupclient"
	FinalizerNodeAgent      = "cybericebox.com/node-agent"
	FinalizerOVSCleanup     = "cybericebox.com/ovs-cleanup"
)
