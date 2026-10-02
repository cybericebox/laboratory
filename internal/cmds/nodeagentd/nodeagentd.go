//go:build linux

package nodeagentd

import (
	"github.com/cybericebox/laboratory/internal/errorlog"
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/deviceplugin"
	"github.com/cybericebox/laboratory/internal/imagepull"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodeagent"
	"github.com/cybericebox/laboratory/internal/profiles"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func Run() {
	journalLog, journal := errorlog.Setup("node-agent", errorlog.Instance(), zap.New())
	ctrl.SetLogger(journalLog)
	log := ctrl.Log.WithName("node-agent")

	cfg, err := nodeagent.LoadConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	ovs, err := nodeagent.NewOVSManager(cfg.Bridge, cfg.OVSSock)
	if err != nil {
		log.Error(err, "init OVS manager")
		os.Exit(1)
	}
	defer ovs.Close()
	ovs.SetPolicing(cfg.PortPolicingKbps)

	ovsRunDir := filepath.Dir(cfg.OVSSock)
	flows, err := nodeagent.NewFlowManager(ovsRunDir, cfg.Bridge)
	if err != nil {
		log.Error(err, "init flow manager")
		os.Exit(1)
	}
	defer flows.Close()

	// OVS is the node's datapath: when either channel to it is lost, exit so that the kubelet restarts the node-agent and the bridge is
	// programmed again (the channels do not reconnect).
	go nodeagent.Watchdog(context.Background(), cfg.OVSWatchInterval, cfg.OVSWatchFailures,
		func(ctx context.Context) error {
			if err := ovs.Ping(ctx); err != nil {
				return fmt.Errorf("OVSDB: %w", err)
			}
			if err := flows.Ping(); err != nil {
				return fmt.Errorf("OpenFlow: %w", err)
			}
			return nil
		},
		func(err error) {
			log.Error(err, "lost OVS: exiting to be restarted")
			os.Exit(1)
		})

	grpcSrv := nodeagent.NewNodeAgentServer(ovs, flows)
	gs, err := nodeagent.StartGRPCServer(cfg.GRPCSock, grpcSrv)
	if err != nil {
		log.Error(err, "start gRPC server")
		os.Exit(1)
	}
	defer gs.GracefulStop()

	nodeAddr, err := nodeAddress()
	if err != nil {
		log.Error(err, "determine node address")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(
		ctrl.GetConfigOrDie(), ctrl.Options{
			Scheme: scheme,
			// No metrics endpoint: the node-agent is on the host network, so it would be open on the node's address.
			Metrics: metricsserver.Options{BindAddress: "0"},
		},
	)
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}
	grpcSrv.SetK8sClient(mgr.GetClient())

	if err := (&nodeagent.DevicePortReconciler{
		Client:      mgr.GetClient(),
		NodeName:    cfg.NodeName,
		NodeAddress: nodeAddr,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup DevicePortReconciler")
		os.Exit(1)
	}

	// Geneve (UDP 6081) is accepted only from the nodes of the cluster.
	if err := mgr.Add(&nodeagent.GeneveSourceSync{Reader: mgr.GetAPIReader(), Flows: flows}); err != nil {
		log.Error(err, "setup GeneveSourceSync")
		os.Exit(1)
	}

	if err := (&nodeagent.ConnectionReconciler{
		Client:      mgr.GetClient(),
		NodeName:    cfg.NodeName,
		NodeAddress: nodeAddr,
		OVS:         ovs,
		Flows:       flows,
		Recorder:    mgr.GetEventRecorderFor("node-agent-connection"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup ConnectionReconciler")
		os.Exit(1)
	}

	if err := (&nodeagent.NetworkAttachReconciler{
		Client:   mgr.GetClient(),
		NodeName: cfg.NodeName,
		OVS:      ovs,
		Flows:    flows,
		CRISock:  cfg.CRISock,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup NetworkAttachReconciler")
		os.Exit(1)
	}

	puller, err := imagepull.NewCRIPuller(cfg.CRISock)
	if err != nil {
		log.Error(err, "connect to the container runtime")
		os.Exit(1)
	}
	defer puller.Close()
	if err := (&nodeagent.ImagePullReconciler{
		Client:          mgr.GetClient(),
		Reader:          mgr.GetAPIReader(),
		NodeName:        cfg.NodeName,
		ImagesNamespace: names.ImagesNamespace,
		Puller:          puller,
		Concurrency:     cfg.ImagePullConcurrency,
		Timeout:         cfg.ImagePullTimeout,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup ImagePullReconciler")
		os.Exit(1)
	}

	if err := nodeagent.SetupDeviceState(mgr, cfg); err != nil {
		log.Error(err, "setup device state persistence")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// The device plugin that lets `extended` device pods have /dev/net/tun (resource cybericebox.com/tun).
	// It is optional: this process is also the CNI, so a failure here (no kubelet directory, no socket) is only logged and retried;
	// it never stops the node-agent. Without the plugin only the tun device of the `extended` profile is missing.
	tun := &deviceplugin.Plugin{Resource: profiles.TUNResource, Dir: cfg.DevicePluginDir, Slots: cfg.TunSlots, CheckPath: cfg.TunCheckPath, Log: log.WithName("device-plugin")}
	go func() {
		for ctx.Err() == nil {
			if err := tun.Serve(ctx); err != nil {
				log.Error(err, "tun device plugin stopped; extended devices will not get /dev/net/tun until it runs (retry in 30s)")
			}
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
		}
	}()

	log.Info("starting node-agent", "node", cfg.NodeName, "bridge", cfg.Bridge)
	errorlog.Start(ctx, journal, ctrl.GetConfigOrDie(), errorlog.Namespace("laboratory-system"), ctrl.Log.WithName("error-journal"))
	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "manager error: %v\n", err)
		os.Exit(1)
	}
}

// containerIfacePrefixes lists interface name prefixes created by container runtimes and overlay
// networks. These must not be used as the Geneve VTEP address.
var containerIfacePrefixes = []string{"docker", "cni", "flannel", "weave", "veth", "br-", "ovs"}

// nodeAddress returns the first non-loopback, non-container-bridge IPv4 of this host (Geneve VTEP).
// Can be overridden with NODE_ADDRESS env var.
func nodeAddress() (string, error) {
	if addr := os.Getenv("NODE_ADDRESS"); addr != "" {
		return addr, nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		skip := false
		for _, prefix := range containerIfacePrefixes {
			if strings.HasPrefix(iface.Name, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, _ := net.ParseCIDR(addr.String())
			if ip != nil && ip.To4() != nil {
				return ip.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address found; set NODE_ADDRESS env")
}
