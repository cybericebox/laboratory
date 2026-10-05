//go:build !linux

package cnigate

import "errors"

func routeAccessPort(string, string) error {
	return errors.New("source routing of the access port needs Linux")
}
