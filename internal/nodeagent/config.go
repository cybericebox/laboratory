//go:build linux

package nodeagent

import (
	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	NodeName string `env:"NODE_NAME,required"`
	OVSSock  string `env:"OVS_SOCK"   envDefault:"/run/openvswitch/db.sock"`
	GRPCSock string `env:"GRPC_SOCK"  envDefault:"/run/cybericebox/node-agent.sock"`
	Bridge   string `env:"OVS_BRIDGE" envDefault:"br-ovs"`
	CRISock  string `env:"CRI_SOCK"   envDefault:"/run/containerd/containerd.sock"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
