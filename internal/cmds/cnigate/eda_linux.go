//go:build linux

package cnigate

import (
	"fmt"

	"github.com/cybericebox/laboratory/internal/edaroute"
)

// routeEDA moves the routes of the EDA interface out of the main table of the pod netns (see package edaroute) and
// checks that none is left.
func routeEDA(netns, iface string) error {
	if _, err := edaroute.Ensure(netns, iface, nil); err != nil {
		return err
	}
	left, err := edaroute.MainRoutes(netns, iface)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("%d routes through %s are still in the main table", len(left), iface)
	}
	return nil
}
