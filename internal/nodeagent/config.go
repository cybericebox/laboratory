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

	// Device state persistence. Empty StateRegistryAddr switches it off.
	//
	// StateRegistryAddr is host:port of the snapshot registry Service. The
	// node-agent relays it to 127.0.0.1:StateForwardPort, so snapshot images are
	// referenced as localhost:<port>/... and pulled by the node's containerd
	// over plain HTTP without any host configuration.
	StateRegistryAddr     string `env:"STATE_REGISTRY_ADDR"`
	StateRegistryUser     string `env:"STATE_REGISTRY_USER"`
	StateRegistryPassword string `env:"STATE_REGISTRY_PASSWORD"`
	StateForwardPort      int    `env:"STATE_FORWARD_PORT"    envDefault:"5035"`
	// ContainerdNamespace is the containerd namespace of the kubelet's CRI.
	ContainerdNamespace string `env:"CONTAINERD_NAMESPACE" envDefault:"k8s.io"`
	// CgroupRoot is where the host's cgroup v2 tree is mounted in the pod.
	CgroupRoot string `env:"CGROUP_ROOT" envDefault:"/host/sys/fs/cgroup"`
	// StateWorkDir holds the temporary layer files of a snapshot.
	StateWorkDir string `env:"STATE_WORK_DIR" envDefault:"/var/cache/cybericebox/state"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	return cfg, config.Load(cfg)
}
