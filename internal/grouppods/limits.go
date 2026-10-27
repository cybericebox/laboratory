package grouppods

import "fmt"

// Limits are the chart's groupLimits: the most a group may be made for, and what it is made for when nobody says (env
// GROUP_MAX_USERS, GROUP_MAX_LABS, GROUP_DEFAULT_USERS, GROUP_DEFAULT_LABS). Over the maximum the agent refuses to create the group.
type Limits struct {
	MaxUsers     int `env:"GROUP_MAX_USERS" envDefault:"10"`
	MaxLabs      int `env:"GROUP_MAX_LABS" envDefault:"50"`
	DefaultUsers int `env:"GROUP_DEFAULT_USERS" envDefault:"1"`
	DefaultLabs  int `env:"GROUP_DEFAULT_LABS" envDefault:"5"`
}

// Validate checks that the maxima are positive and the defaults within them.
func (l Limits) Validate() error {
	if l.MaxUsers <= 0 || l.MaxLabs <= 0 {
		return fmt.Errorf("GROUP_MAX_USERS and GROUP_MAX_LABS must be positive")
	}
	if l.DefaultUsers <= 0 || l.DefaultUsers > l.MaxUsers || l.DefaultLabs <= 0 || l.DefaultLabs > l.MaxLabs {
		return fmt.Errorf("GROUP_DEFAULT_USERS and GROUP_DEFAULT_LABS must be positive and within the maxima")
	}
	return nil
}

// Resolve gives the counts a group is made for: the asked ones, else the defaults, and refuses what is over the maximum or negative.
func (l Limits) Resolve(users, labs int) (int, int, error) {
	if users < 0 || labs < 0 {
		return 0, 0, fmt.Errorf("expected_users and expected_labs cannot be negative")
	}
	if users == 0 {
		users = l.DefaultUsers
	}
	if labs == 0 {
		labs = l.DefaultLabs
	}
	if users > l.MaxUsers {
		return 0, 0, fmt.Errorf("expected_users %d is over the maximum %d of a group", users, l.MaxUsers)
	}
	if labs > l.MaxLabs {
		return 0, 0, fmt.Errorf("expected_labs %d is over the maximum %d of a group", labs, l.MaxLabs)
	}
	return users, labs, nil
}

// The formulas that size the pods of a group from the users (VPN clients) and the labs it is made for. The coefficients are
// MEASURED on kind (2026-10-27, laboratory v1.7.x + this branch, an idle control plane: the data path of WireGuard and of the NAT is
// the kernel's and is not in the pod's cgroup), with 50 % added, and they live in code, not in the chart:
//
//	VPN pod:     memory 9 Mi at no clients; +0.6 Mi per client; +0.1 Mi per lab; +0.016 Mi per access rule (clients x labs)
//	             CPU    2m idle; +0.3m per client; +0.1m per lab; a burst of about 0.12m per access rule while the rules are applied
//	             (10 clients x 50 labs = 480 rules: 27 Mi steady, 72m for the minute the rules were applied, then 1m)
//	gateway pod: memory 8 Mi at no labs, +0.15 Mi per lab that uses the internet (50 labs: 15 Mi); CPU 1m idle, 7-9m while labs attach
//
// The sizes are floors-and-steps: never below a floor that keeps the Go process safe from its own allocator, rounded up to a step.
const (
	vpnMemBase, vpnMemPerUser, vpnMemPerLab, vpnMemPerRule = 9 << 20, 614 << 10, 102 << 10, 16 << 10 // bytes
	vpnCPUBase, vpnCPUPerUser, vpnCPUPerLab, vpnCPUPerRule = 5000, 300, 100, 120                     // micro-millicores (1/1000 m)
	gwMemBase, gwMemPerLab                                 = 8 << 20, 154 << 10                      // bytes
	gwCPUBase, gwCPUPerLab                                 = 5000, 250                               // micro-millicores
	sizeMarginNum, sizeMarginDen                           = 3, 2                                    // +50 %
	memFloorVPN, memFloorGateway                           = 24 << 20, 16 << 20
	cpuFloor                                               = 10 // millicores
	memStep                                                = 4 << 20
)

func marginUp(v int64) int64 { return (v*sizeMarginNum + sizeMarginDen - 1) / sizeMarginDen }

func ceilTo(v, step int64) int64 {
	if r := v % step; r != 0 {
		return v + step - r
	}
	return v
}

// VPNSize is the size of the VPN pod of a group made for `users` clients and `labs` labs (rules = users x labs).
func VPNSize(users, labs int) Overhead {
	u, l := int64(users), int64(labs)
	mem := marginUp(vpnMemBase + vpnMemPerUser*u + vpnMemPerLab*l + vpnMemPerRule*u*l)
	cpu := marginUp(vpnCPUBase + vpnCPUPerUser*u + vpnCPUPerLab*l + vpnCPUPerRule*u*l)
	return Overhead{CPU: max(cpuFloor, (cpu+999)/1000), Memory: max(memFloorVPN, ceilTo(mem, memStep))}
}

// GatewaySize is the size of the gateway pod of a group made for `labs` labs.
func GatewaySize(labs int) Overhead {
	l := int64(labs)
	mem := marginUp(gwMemBase + gwMemPerLab*l)
	cpu := marginUp(gwCPUBase + gwCPUPerLab*l)
	return Overhead{CPU: max(cpuFloor, (cpu+999)/1000), Memory: max(memFloorGateway, ceilTo(mem, memStep))}
}
