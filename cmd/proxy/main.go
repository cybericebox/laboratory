// proxy is the internet-facing image's binary: `proxy proxy-l7` and `proxy proxy-wg`.
package main

import (
	"github.com/cybericebox/laboratory/internal/cmds/proxyl7"
	"github.com/cybericebox/laboratory/internal/multicall"
)

var commands = map[string]func(){"proxy-l7": proxyl7.Run}

func main() { multicall.Run(commands) }
