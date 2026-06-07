//go:build linux

package main

import (
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	nodeagent "github.com/cybericebox/laboratory/internal/nodeagent"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func main() {
	ctrl.SetLogger(zap.New())
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

	ovsRunDir := filepath.Dir(cfg.OVSSock)
	flows, err := nodeagent.NewFlowManager(ovsRunDir, cfg.Bridge)
	if err != nil {
		log.Error(err, "init flow manager")
		os.Exit(1)
	}
	defer flows.Close()

	grpcSrv := nodeagent.NewNodeAgentServer(ovs)
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
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

	if err := (&nodeagent.ConnectionReconciler{
		Client:      mgr.GetClient(),
		NodeName:    cfg.NodeName,
		NodeAddress: nodeAddr,
		OVS:         ovs,
		Flows:       flows,
		ProcRoot:    cfg.ProcRoot,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup ConnectionReconciler")
		os.Exit(1)
	}

	if err := (&nodeagent.NetworkAttachReconciler{
		Client:   mgr.GetClient(),
		NodeName: cfg.NodeName,
		OVS:      ovs,
		Server:   grpcSrv,
		ProcRoot: cfg.ProcRoot,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setup NetworkAttachReconciler")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log.Info("starting node-agent", "node", cfg.NodeName, "bridge", cfg.Bridge)
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
