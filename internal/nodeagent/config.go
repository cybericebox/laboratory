//go:build linux

package nodeagent

import (
	"time"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	NodeName string `env:"NODE_NAME,required"`
	OVSSock  string `env:"OVS_SOCK"   envDefault:"/run/openvswitch/db.sock"`
	GRPCSock string `env:"GRPC_SOCK"  envDefault:"/run/cybericebox/node-agent.sock"`
	Bridge   string `env:"OVS_BRIDGE" envDefault:"br-ovs"`
	// HealthAddr is where the readiness and liveness probes are served. The node-agent is on the host network, so it is the loopback
	// only (the probes name host 127.0.0.1): the port is not open on the node's address.
	HealthAddr string `env:"HEALTH_ADDR" envDefault:"127.0.0.1:9440"`
	CRISock    string `env:"CRI_SOCK"   envDefault:"/run/k0s/containerd.sock"`
	// DevicePluginDir is the kubelet's device-plugins directory (the kubelet hardcodes /var/lib/kubelet/device-plugins, also on k0s);
	// the node-agent registers the extended resource cybericebox.com/tun there. TunSlots is how many devices of that
	// resource a node advertises (each is the same /dev/net/tun, the count only bounds the extended devices per node).
	DevicePluginDir string `env:"DEVICE_PLUGIN_DIR" envDefault:"/var/lib/kubelet/device-plugins"`
	// TunCheckPath is what the plugin looks at to see that the tun device exists on the HOST: the sysfs entry of the tun misc device
	// (the container's own /dev has no tun even when the host does).
	TunCheckPath string `env:"TUN_CHECK_PATH"   envDefault:"/sys/class/misc/tun/dev"`
	TunSlots     int    `env:"TUN_SLOTS"         envDefault:"1000"`
	// PortPolicingKbps is the rate (kbit/s) a device's veth port may send into the bridge: the storm control of the shared switch
	// (0 = off). OVSWatchInterval is how often the node-agent asks OVS (database and OpenFlow) for a sign of life; after
	// OVSWatchFailures misses in a row it exits so that it is restarted and the bridge is programmed again.
	PortPolicingKbps int           `env:"PORT_POLICING_KBPS" envDefault:"500000"`
	OVSWatchInterval time.Duration `env:"OVS_WATCH_INTERVAL" envDefault:"15s"`
	OVSWatchFailures int           `env:"OVS_WATCH_FAILURES" envDefault:"3"`
	// ImagePullConcurrency is how many images of a prepull request the node pulls at once;
	// ImagePullTimeout bounds one image.
	ImagePullConcurrency int           `env:"IMAGE_PULL_CONCURRENCY" envDefault:"2"`
	ImagePullTimeout     time.Duration `env:"IMAGE_PULL_TIMEOUT"     envDefault:"5m"`

	// The platform registry (zot): the snapshots of device state and the image cache.
	//
	// StateRegistryAddr is host:port of the registry Service (empty = names.RegistryServiceAddr: the chart always installs it there).
	// The node-agent relays it to 127.0.0.1:StateForwardPort (names.RegistryForwardPort, a constant), so snapshot and cached images are
	// referenced as localhost:<port>/... and pulled by the node's containerd over plain HTTP without any host configuration.
	StateRegistryAddr     string `env:"STATE_REGISTRY_ADDR"`
	StateRegistryUser     string `env:"STATE_REGISTRY_USER"`
	StateRegistryPassword string `env:"STATE_REGISTRY_PASSWORD"`
	StateForwardPort      int
	// The reader account of the registry (it may read the snapshots and the base repository; the forwarder adds it to the
	// node runtime's pulls of those). Empty: the forwarder is a plain relay.
	StateRegistryReaderUser     string `env:"STATE_REGISTRY_READER_USER"`
	StateRegistryReaderPassword string `env:"STATE_REGISTRY_READER_PASSWORD"`
	// StatePersistence turns on the snapshot engine (the forwarder alone serves
	// the image cache).
	StatePersistence bool `env:"STATE_PERSISTENCE_ENABLED" envDefault:"false"`
	// ContainerdNamespace is the containerd namespace of the kubelet's CRI.
	ContainerdNamespace string `env:"CONTAINERD_NAMESPACE" envDefault:"k8s.io"`
	// CgroupRoot is where the host's cgroup v2 tree is mounted in the pod.
	CgroupRoot string `env:"CGROUP_ROOT" envDefault:"/host/sys/fs/cgroup"`
	// StateMaxWatchDirs is how many directories of one device's writable layer are watched with inotify; past it the layer is
	// only polled.
	StateMaxWatchDirs int `env:"STATE_MAX_WATCH_DIRS" envDefault:"2000"`
	// What one device may push (R-5): the least time between two pushes, the most state in StatePushBudgetWindow (a Kubernetes
	// quantity; "0" = no budget), and how long the replaced manifest of a device is kept before it is deleted.
	StateMinPushInterval  time.Duration `env:"STATE_MIN_PUSH_INTERVAL" envDefault:"60s"`
	StatePushBudget       string        `env:"STATE_PUSH_BUDGET" envDefault:"2Gi"`
	StatePushBudgetWindow time.Duration `env:"STATE_PUSH_BUDGET_WINDOW" envDefault:"1h"`
	StateSupersededGrace  time.Duration `env:"STATE_SUPERSEDED_GRACE" envDefault:"2m"`
	// StateRegistryCapacity is the size of the registry volume (a quantity; empty = unknown, no refusal) and
	// StateRegistryReserve the fraction of it that is kept free: a push that would pass that line is refused.
	StateRegistryCapacity string  `env:"STATE_REGISTRY_CAPACITY"`
	StateRegistryReserve  float64 `env:"STATE_REGISTRY_RESERVE" envDefault:"0.1"`
	// StateWorkDir holds the temporary layer files of a snapshot.
	StateWorkDir string `env:"STATE_WORK_DIR" envDefault:"/var/cache/cybericebox/state"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{StateForwardPort: names.RegistryForwardPort}
	if err := config.Load(cfg); err != nil {
		return cfg, err
	}
	if cfg.StateRegistryAddr == "" {
		cfg.StateRegistryAddr = names.RegistryServiceAddr
	}
	return cfg, nil
}
