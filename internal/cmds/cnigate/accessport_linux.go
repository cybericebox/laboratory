//go:build linux

package cnigate

import (
	"fmt"

	"github.com/cybericebox/laboratory/internal/accessroute"
)

// routeAccessPort moves the routes of the access port out of the main table of the pod netns (see package accessroute) and
// checks that none is left.
func routeAccessPort(netns, iface string) error {
	if _, err := accessroute.Ensure(netns, iface, nil); err != nil {
		return err
	}
	left, err := accessroute.MainRoutes(netns, iface)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("%d routes through %s are still in the main table", len(left), iface)
	}
	return nil
}
