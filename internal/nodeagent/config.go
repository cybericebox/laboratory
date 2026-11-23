//go:build linux

package nodeagent

import (
	"fmt"
	"time"

	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/config"
)

type Config struct {
	// Bandwidth are the chart's bandwidth values (BANDWIDTH_*): with it on, the Geneve traffic of the node is held to its class by
	// OVS meters (see package bandwidth).
	Bandwidth                  bandwidth.Settings
	RuntimeObservation         bool          `env:"RUNTIME_OBSERVATION_ENABLED" envDefault:"false"`
	RuntimeObservationInterval time.Duration `env:"RUNTIME_OBSERVATION_INTERVAL" envDefault:"5s"`
	NodeName                   string        `env:"NODE_NAME,required"`
	OVSSock                    string        `env:"OVS_SOCK"   envDefault:"/run/openvswitch/db.sock"`
	GRPCSock                   string        `env:"GRPC_SOCK"  envDefault:"/run/cybericebox/node-agent.sock"`
	Bridge                     string        `env:"OVS_BRIDGE" envDefault:"br-ovs"`
	// HealthAddr is where the readiness and liveness probes are served. The node-agent is on the host network, so it is the loopback
	// only (the probes name host 127.0.0.1): the port is not open on the node's address.
	HealthAddr string `env:"HEALTH_ADDR" envDefault:"127.0.0.1:9440"`
	// MetricsAddr is where the Prometheus endpoint is served ("0": none, the default). It is on the host network, so an address without a
	// host is open on the node's address: the chart sets it only with nodeAgent.metrics.enabled.
	MetricsAddr string `env:"METRICS_ADDR" envDefault:"0"`
	CRISock     string `env:"CRI_SOCK"   envDefault:"/run/k0s/containerd.sock"`
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

	// Console cables (chart consoleCables.enabled): the node-agent drives the console lines of this node through the device access service
	// of the node (CableSocket, cable.sock), and finds the device access pods of the other nodes in DeviceAccessNamespace, whose cable
	// stream listens on ConsoleCablePort.
	ConsoleCables         bool   `env:"CONSOLE_CABLES_ENABLED" envDefault:"false"`
	CableSocket           string `env:"CABLE_SOCKET" envDefault:"/run/cybericebox/console-cables/cable.sock"`
	DeviceAccessNamespace string `env:"DEVICE_ACCESS_NAMESPACE" envDefault:"laboratory-device-access"`
	ConsoleCablePort      int    `env:"CONSOLE_CABLE_PORT" envDefault:"8081"`

	// The platform registry (zot): the snapshots of device state and the image cache.
	//
	// StateRegistryAddr is host:port of the registry gate's TLS port (empty = names.RegistryGateAddr: the chart always installs it there).
	// The node-agent relays it to 127.0.0.1:StateForwardPort (names.RegistryForwardPort, a constant), so snapshot and cached images are
	// referenced as localhost:<port>/... and pulled by the node's containerd over plain HTTP without any host configuration.
	StateRegistryAddr string `env:"STATE_REGISTRY_ADDR"`
	// StateRegistryCAFile, when set, makes the relay speak TLS to the gate (its address is then the gate's TLS port) and trust only this
	// certificate authority (the chart mounts the ConfigMap laboratory-registry-ca there).
	StateRegistryCAFile string `env:"STATE_REGISTRY_CA_FILE"`
	// RegistryEnabled is false when the chart installs no platform registry (no image cache, no state persistence, no platform
	// images): the relay is not started and there is no registry address. Default true (a registry exists, as before).
	RegistryEnabled  bool `env:"REGISTRY_ENABLED" envDefault:"true"`
	StateForwardPort int
	// RegistryInsecureDev (REGISTRY_INSECURE_DEV) is the development setup: plain http to the registry and no token at all. Without a
	// certificate authority and without it the node-agent refuses to start the relay: the node's token never travels in the clear.
	RegistryInsecureDev bool `env:"REGISTRY_INSECURE_DEV" envDefault:"false"`
	// RegistryTokenFile is the projected ServiceAccount token (audience laboratory-registry-gate) the relay presents to the gate in
	// X-Cib-Node-Token on every request. A node has no registry account and no registry password.
	RegistryTokenFile string `env:"REGISTRY_TOKEN_FILE" envDefault:"/var/run/secrets/registry-gate/token"`
	// StatePushSocket is the unix socket of the relay on which node-state pushes the snapshots (mode 0600); StatePushSocketUID is the
	// user it is given to (the user of node-state; -1: the owner stays the node-agent's).
	StatePushSocket    string `env:"STATE_PUSH_SOCKET" envDefault:"/run/cybericebox/registry-push/push.sock"`
	StatePushSocketUID int    `env:"STATE_PUSH_SOCKET_UID" envDefault:"-1"`
	// StatePersistence turns on the snapshot engine (the forwarder alone serves
	// the image cache).
	StatePersistence bool `env:"STATE_PERSISTENCE_ENABLED" envDefault:"false"`
	// ContainerdRoot is the containerd root directory of the host as mounted in the pod (the same path): the snapshots of a device's
	// writable layer are read from its snapshotter. The preflight checks it.
	ContainerdRoot string `env:"CONTAINERD_ROOT"`
	// ContainerdNamespace is the containerd namespace of the kubelet's CRI.
	ContainerdNamespace string `env:"CONTAINERD_NAMESPACE" envDefault:"k8s.io"`
	// CgroupRoot is where the host's cgroup v2 tree is mounted in the pod.
	CgroupRoot string `env:"CGROUP_ROOT" envDefault:"/host/sys/fs/cgroup"`
	// StateMaxWatchDirs is how many directories of ALL the writable layers of this node are watched with inotify together (one budget
	// shared by every device); past it a layer is only polled.
	StateMaxWatchDirs int `env:"STATE_MAX_WATCH_DIRS" envDefault:"2000"`
	// StateMaxConcurrentDiffs is how many writable layers the node diffs at once; StateTempBudget (a Kubernetes quantity; empty = none) is
	// how many bytes of temporary layer files in StateWorkDir all the running diffs may hold together.
	StateMaxConcurrentDiffs int    `env:"STATE_MAX_CONCURRENT_DIFFS" envDefault:"2"`
	StateTempBudget         string `env:"STATE_TEMP_BUDGET"`
	// StateWorkDir holds the temporary layer files of a snapshot.
	StateWorkDir string `env:"STATE_WORK_DIR" envDefault:"/var/cache/cybericebox/state"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{StateForwardPort: names.RegistryForwardPort}
	if err := config.Load(cfg); err != nil {
		return cfg, err
	}
	if !cfg.RegistryEnabled {
		if cfg.StatePersistence {
			return cfg, fmt.Errorf("STATE_PERSISTENCE_ENABLED needs the platform registry, but REGISTRY_ENABLED is false")
		}
		cfg.StateRegistryAddr = "" // no registry: SetupDeviceState starts no relay
		return cfg, nil
	}
	if cfg.StateRegistryAddr == "" {
		cfg.StateRegistryAddr = names.RegistryGateAddr
	}
	return cfg, nil
}

// PreflightInput is the part of the configuration the start-up check reads.
func (c *Config) PreflightInput() PreflightInput {
	return PreflightInput{
		Persistence: c.StatePersistence, Observation: c.RuntimeObservation,
		ContainerdRoot: c.ContainerdRoot, CRISock: c.CRISock, CgroupRoot: c.CgroupRoot, RegistryAddr: c.StateRegistryAddr,
		RegistryCAFile: c.StateRegistryCAFile, RegistryTokenFile: c.RegistryTokenFile, RegistryInsecureDev: c.RegistryInsecureDev,
	}
}

// Apply switches off in the configuration what the node cannot do, so the node-agent starts without it instead of failing.
func (c *Config) Apply(r PreflightResult) {
	if _, bad := IsUnavailable(r.StatePersistence); bad {
		c.StatePersistence = false
	}
	if _, bad := IsUnavailable(r.RuntimeObservation); bad {
		c.RuntimeObservation = false
	}
}
