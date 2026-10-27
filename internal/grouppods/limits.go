package grouppods

import "fmt"

// Limits are the chart's groupLimits: the most a group may be made for, and what it is made for when nobody says (env
// GROUP_MAX_USERS, GROUP_MAX_LABS, GROUP_DEFAULT_USERS, GROUP_DEFAULT_LABS). Over the maximum the agent refuses to create the group.
type Limits struct {
	MaxUsers     int `env:"GROUP_MAX_USERS" envDefault:"10"`
	MaxLabs      int `env:"GROUP_MAX_LABS" envDefault:"50"`
	DefaultUsers int `env:"GROUP_DEFAULT_USERS" envDefault:"1"`
	DefaultLabs  int `env:"GROUP_DEFAULT_LABS" envDefault:"5"`
	// Measured: the formulas (VPNSize, GatewaySize) are trusted to size a pod. Until it is true they are measured without live traffic only, and
	// no pod is made smaller than the chart's default size (vpn.resources, inetGateway.resources) (GROUP_SIZING_MEASURED, chart groupSizing.measured).
	Measured bool `env:"GROUP_SIZING_MEASURED" envDefault:"false"`
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
// MEASURED on kind (2026-10-27, laboratory v1.7.x + this branch), with 50 % added, and they live in code, not in the chart. First
// without traffic (an idle control plane), then with live traffic (10 real WireGuard clients through the VPN pod to 50 labs: iperf3 TCP 1 and
// 4 streams, UDP, a SYN scan of every port, and 25 of the labs through the gateway NAT to an outside host; see the report):
//
//	VPN pod:     idle     memory 9 Mi at no clients; +0.1 Mi per lab; +0.016 Mi per access rule; CPU 2m, a burst of about 0.12m per rule
//	                      while the rules are applied (10 clients x 50 labs = 480 rules: 27 Mi, 72m for the minute of applying, then 1m)
//	             traffic  at 10 clients x 50 labs: peak 42 Mi (anon 32, kernel 4-6; socket memory under 0.1 Mi: the WireGuard and the
//	                      forwarding memory is not charged to the pod) and peak 146m CPU at 4 TCP streams per client (about 1.8 Gbit/s in all; the
//	                      peak of four runs, CPU varies 100-146m from run to run); folded in as +1.5 Mi and +7.6m per client on top of the idle figures
//	gateway pod: idle 8 Mi, 1m; with traffic and NAT of 20 000 connections at peak 19.6 Mi and 6m: +0.23 Mi per lab (50 labs);
//	             the conntrack entries of the NAT (20 000) are not in the pod's cgroup either
//
// The sizes are floors-and-steps: never below a floor that keeps the Go process safe from its own allocator, rounded up to a step.
const (
	vpnMemBase, vpnMemPerUser, vpnMemPerLab, vpnMemPerRule = 9 << 20, 2150 << 10, 102 << 10, 16 << 10 // bytes
	vpnCPUBase, vpnCPUPerUser, vpnCPUPerLab, vpnCPUPerRule = 5000, 7600, 100, 120                     // micro-millicores (1/1000 m)
	gwMemBase, gwMemPerLab                                 = 8 << 20, 240 << 10                       // bytes
	gwCPUBase, gwCPUPerLab                                 = 5000, 250                                // micro-millicores
	sizeMarginNum, sizeMarginDen                           = 3, 2                                     // +50 %
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
