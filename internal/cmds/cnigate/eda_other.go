//go:build !linux

package cnigate

import "errors"

func routeEDA(string, string) error {
	return errors.New("source routing of the EDA interface needs Linux")
}
