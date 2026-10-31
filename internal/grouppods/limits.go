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

// The formulas that size the pods of a group from the users (VPN clients) and the labs it is made for. Their coefficients are a property
// of this binary, not of the cluster, so they live here and not in the chart.
//
// What was measured, and the margin (never below 35 %):
//
//   - kind 2026-10-27, 10 real WireGuard clients, 50 labs, 500 access rules, with traffic: live peaks of the VPN pod 43 Mi and 146m. The
//     per-lab, per-rule and CPU per-client coefficients below are the per-unit costs of that run with +50 % on top (CPU per client 7.6m ->
//     11.4m, per lab 0.1m -> 0.15m, per rule 0.12m -> 0.18m; memory per lab 0.1 Mi -> 0.15 Mi, per rule 0.016 Mi -> 0.024 Mi).
//   - the older local stand, clients pulling pages in a loop: 5 clients 48m and 155 Mi, 10 clients 55m and 195 Mi. That is a line of 8 Mi
//     per client on a base of 115 Mi (the traffic of the clients, not the idle pod); the line that keeps +35 % over both points is 156 Mi +
//     10.8 Mi per client (5 clients 209 Mi, 10 clients 263 Mi): base 156 Mi, 11 Mi per client.
//   - AWS vpn-pooling-20261007, 20 connected clients per group, under UDP/TCP load: 2-6m and 14-25 Mi per VPN pod; an empty group 1.1m
//     and 10 Mi. The memory base is far over it on purpose: the pod is Guaranteed, its limit is its request, and the OOM killer takes the
//     whole VPN of a group; the kind and AWS runs did not push the traffic the local stand did.
//   - CPU: the idle base measured on kind was 8m; the base is 1.5 x that, 12m.
//
// The formulas:
//
//	VPN pod:     memory 156 Mi + 11 Mi per client + 0.15 Mi per lab + 0.024 Mi per access rule (clients x labs)
//	             CPU    12m    + 11.4m per client  + 0.15m per lab   + 0.18m per access rule
//	gateway pod: memory 32 Mi + 0.36 Mi per lab that uses the internet; CPU 10m + 0.375m per lab (unchanged: base = what it had)
//
// Memory is rounded up to a step. (10 clients, 50 labs): 288 Mi (195 Mi seen, +48 %) and 224m (146m seen, +53 %).
const (
	vpnMemBase, vpnMemPerUser, vpnMemPerLab, vpnMemPerRule = 156 << 20, 11534336, 157287, 25166 // bytes
	vpnCPUBase, vpnCPUPerUser, vpnCPUPerLab, vpnCPUPerRule = 12000, 11400, 150, 180             // micro-millicores (1/1000 m)
	gwMemBase, gwMemPerLab                                 = 32 << 20, 377488                   // bytes
	gwCPUBase, gwCPUPerLab                                 = 10000, 375                         // micro-millicores
	memStep                                                = 4 << 20
)

func ceilTo(v, step int64) int64 {
	if r := v % step; r != 0 {
		return v + step - r
	}
	return v
}

// VPNSize is the size of the VPN pod of a group made for `users` clients and `labs` labs (rules = users x labs).
func VPNSize(users, labs int) Overhead {
	u, l := int64(users), int64(labs)
	mem := vpnMemBase + vpnMemPerUser*u + vpnMemPerLab*l + vpnMemPerRule*u*l
	cpu := vpnCPUBase + vpnCPUPerUser*u + vpnCPUPerLab*l + vpnCPUPerRule*u*l
	return Overhead{CPU: (cpu + 999) / 1000, Memory: ceilTo(mem, memStep)}
}

// GatewaySize is the size of the gateway pod of a group made for `labs` labs.
func GatewaySize(labs int) Overhead {
	l := int64(labs)
	mem := gwMemBase + gwMemPerLab*l
	cpu := gwCPUBase + gwCPUPerLab*l
	return Overhead{CPU: (cpu + 999) / 1000, Memory: ceilTo(mem, memStep)}
}
