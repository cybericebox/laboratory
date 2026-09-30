package main

import (
	"github.com/cybericebox/laboratory/internal/cmds/agent"
	"github.com/cybericebox/laboratory/internal/cmds/cnigate"
	"github.com/cybericebox/laboratory/internal/cmds/manager"
	"github.com/cybericebox/laboratory/internal/cmds/proxyl7"
)

func init() {
	commands["manager"] = manager.Run
	commands["agent"] = agent.Run
	commands["proxy-l7"] = proxyl7.Run
	commands["cni-gate"] = cnigate.Run
}
