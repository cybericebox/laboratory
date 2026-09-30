//go:build linux

package main

import "github.com/cybericebox/laboratory/internal/cmds/proxywg"

func init() { commands["proxy-wg"] = proxywg.Run }
