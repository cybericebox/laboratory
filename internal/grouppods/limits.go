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

// SizeFor is the size of the pod for the given number of units (VPN: users; gateway: labs): the sizing's base plus its per-unit
// values for each unit. The per-unit values are the measured ones (a VPN pod about 5-6m CPU and 18Mi per active peer on top of the
// base process, a gateway about 1-2m and 3-4Mi per lab) with the margin of the chart defaults, so that a group over its expected
// count still runs within the formula.
func (p PodSizing) SizeFor(units int) Overhead {
	n := int64(units)
	return Overhead{CPU: p.BaseCPU + p.PerUnitCPU*n, Memory: p.BaseMemory + p.PerUnitMemory*n}
}
