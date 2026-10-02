//go:build linux

package nodeagent

import (
	"time"

	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	NodeName string `env:"NODE_NAME,required"`
	OVSSock  string `env:"OVS_SOCK"   envDefault:"/run/openvswitch/db.sock"`
	GRPCSock string `env:"GRPC_SOCK"  envDefault:"/run/cybericebox/node-agent.sock"`
	Bridge   string `env:"OVS_BRIDGE" envDefault:"br-ovs"`
	CRISock  string `env:"CRI_SOCK"   envDefault:"/run/k0s/containerd.sock"`
	// DevicePluginDir is the kubelet's device-plugins directory (k0s keeps the kubelet root under /var/lib/k0s/kubelet);
	// the node-agent registers the extended resource cybericebox.com/tun there. TunSlots is how many devices of that
	// resource a node advertises (each is the same /dev/net/tun, the count only bounds the extended devices per node).
	DevicePluginDir string `env:"DEVICE_PLUGIN_DIR" envDefault:"/var/lib/k0s/kubelet/device-plugins"`
	TunSlots        int    `env:"TUN_SLOTS"         envDefault:"1000"`
	// ImagePullConcurrency is how many images of a prepull request the node pulls at once;
	// ImagePullTimeout bounds one image.
	ImagePullConcurrency int           `env:"IMAGE_PULL_CONCURRENCY" envDefault:"2"`
	ImagePullTimeout     time.Duration `env:"IMAGE_PULL_TIMEOUT"     envDefault:"5m"`

	// The platform registry (zot): the snapshots of device state and the image
	// cache. Empty StateRegistryAddr switches both off.
	//
	// StateRegistryAddr is host:port of the registry Service. The node-agent
	// relays it to 127.0.0.1:StateForwardPort, so snapshot and cached images are
	// referenced as localhost:<port>/... and pulled by the node's containerd
	// over plain HTTP without any host configuration.
	StateRegistryAddr     string `env:"STATE_REGISTRY_ADDR"`
	StateRegistryUser     string `env:"STATE_REGISTRY_USER"`
	StateRegistryPassword string `env:"STATE_REGISTRY_PASSWORD"`
	StateForwardPort      int    `env:"STATE_FORWARD_PORT"    envDefault:"5035"`
	// StatePersistence turns on the snapshot engine (the forwarder alone serves
	// the image cache).
	StatePersistence bool `env:"STATE_PERSISTENCE_ENABLED" envDefault:"false"`
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
