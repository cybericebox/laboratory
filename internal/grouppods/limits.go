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
// of this binary, not of the cluster, so they live here and not in the chart. Each coefficient already holds the +50 % margin over what was
// measured on kind (2026-10-27, 10 real WireGuard clients, 50 labs, 500 access rules; live peaks: VPN 43 Mi and 146m, with the idle
// figures below them; gateway 19.6 Mi and 6m), and the base is the size a pod had before the formulas (VPN 100m / 320Mi, gateway
// 10m / 32Mi), so a group is never smaller than it used to be.
//
// Until the load test with traffic is repeated on the platform only the base comes down: what was measured is about 14 Mi and 8m for the
// VPN base and about 12 Mi and 8m for the gateway base.
//
//	VPN pod:     memory 320 Mi + 3.2 Mi per client + 0.15 Mi per lab + 0.024 Mi per access rule (clients x labs)
//	             CPU    100m   + 11.4m per client   + 0.15m per lab   + 0.18m per access rule
//	gateway pod: memory 32 Mi + 0.36 Mi per lab that uses the internet; CPU 10m + 0.375m per lab
//
// Memory is rounded up to a step.
const (
	vpnMemBase, vpnMemPerUser, vpnMemPerLab, vpnMemPerRule = 320 << 20, 3355444, 157287, 25166 // bytes
	vpnCPUBase, vpnCPUPerUser, vpnCPUPerLab, vpnCPUPerRule = 100000, 11400, 150, 180           // micro-millicores (1/1000 m)
	gwMemBase, gwMemPerLab                                 = 32 << 20, 377488                  // bytes
	gwCPUBase, gwCPUPerLab                                 = 10000, 375                        // micro-millicores
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
