//go:build linux

package gateway

import (
	"github.com/cybericebox/laboratory/internal/errorlog"
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/egressproxy"
	"github.com/cybericebox/laboratory/internal/gateway"
	"github.com/cybericebox/laboratory/pkg/dhcp"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(allocationv1alpha1.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func Run() {
	journalLog, journal := errorlog.Setup("gateway", errorlog.Instance(), zap.New())
	ctrl.SetLogger(journalLog)
	log := ctrl.Log.WithName("gateway")

	cfg, err := gateway.LoadConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	ipt, err := gateway.NewIPTablesManager(cfg.ExternalInterface)
	if err != nil {
		log.Error(err, "init iptables")
		os.Exit(1)
	}
	ipt.MaxConnectionsPerLab, ipt.NewConnectionsPerSecondPerLab = cfg.MaxConnectionsPerLab, cfg.NewConnectionsPerSecondPerLab
	ipt.Egress = gateway.EgressPorts{HTTP: cfg.EgressHTTPPort, TLS: cfg.EgressTLSPort, DNS: cfg.EgressDNSPort}
	ipt.ICMPPerSecond = cfg.EgressICMPPerSecond
	defer ipt.RemoveInput()
	if err := ipt.SetupFilter(); err != nil {
		log.Error(err, "setup the forwarding filter")
		os.Exit(1)
	}
	if !ipt.IPv6Filtered {
		log.Info("no ip6tables in this pod: the gateway forwards no IPv6")
	}

	mgr, err := ctrl.NewManager(
		ctrl.GetConfigOrDie(), ctrl.Options{
			Scheme: scheme,
			// No metrics endpoint: it would answer on the pod IP to the labs of the group (and name the API server).
			Metrics: metricsserver.Options{BindAddress: "0"},
			Cache: cache.Options{
				DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}},
			},
		},
	)
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	dhcpMgr := dhcp.NewManager()
	defer dhcpMgr.Close()
	labs := egressproxy.NewLabMap()
	// The listeners stand before the reconciler exists: it installs the redirects of a lab's 80/443/53 only after this returns.
	if err := startEgress(cfg, labs, log); err != nil {
		log.Error(err, "start the egress services")
		os.Exit(1)
	}
	// The bandwidth ceiling of each lab comes from the annotation of this pod (a downward API volume); a pod made without the volume
	// has no ceiling and shapes nothing.
	shaper := gateway.NewShaper()
	// Without a bandwidth annotation the labs still have the ceilings of the chart: what a lab receives, what it sends.
	if err := shaper.SetLimits(cfg.DownloadKbps, cfg.UploadKbps); err != nil {
		log.Error(err, "set the default ceilings")
	}
	if err := (&gateway.LabGatewayReconciler{
		Client:   mgr.GetClient(),
		DHCP:     dhcpMgr,
		IPT:      ipt,
		Shaper:   shaper,
		Labs:     labs,
		Cfg:      cfg,
		Recorder: mgr.GetEventRecorderFor("labgateway"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "LabGateway")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	go func() {
		err := bandwidth.Watch(ctx, bandwidth.AnnotationsFile,
			func(l bandwidth.Limits) {
				down, up := cfg.DownloadKbps, cfg.UploadKbps
				if l.CapKbps > 0 {
					down = l.CapKbps
				}
				if l.UpKbps > 0 {
					up = l.UpKbps
				}
				if err := shaper.SetLimits(down, up); err != nil {
					log.Error(err, "apply the bandwidth ceilings", "downKbps", down, "upKbps", up)
				}
			},
			func(err error) { log.Error(err, "read the bandwidth annotation") })
		if err != nil {
			log.Error(err, "watch the bandwidth annotation")
		}
	}()

	log.Info("starting gateway manager", "namespace", cfg.Namespace)
	errorlog.Start(ctx, journal, ctrl.GetConfigOrDie(), cfg.Namespace, ctrl.Log.WithName("error-journal"))
	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "manager error: %v\n", err)
		os.Exit(1)
	}
}

// startEgress starts the web proxy, the DNS answerer and the summaries page of the gateway. They run for the life of the process.
func startEgress(cfg *gateway.Config, labs *egressproxy.LabMap, log logr.Logger) error {
	upstreams := splitList(cfg.EgressDNSUpstreams)
	resolver, err := egressproxy.NewResolver(upstreams)
	if err != nil {
		return err
	}
	suffixes := egressproxy.DefaultTunnelSuffixes
	switch cfg.EgressTunnelSuffixes {
	case "":
	case "-":
		suffixes = nil
	default:
		suffixes = splitList(cfg.EgressTunnelSuffixes)
	}
	deny := egressproxy.NewDenylist(suffixes)
	pod, _ := os.Hostname()
	stats := egressproxy.NewCollector(pod)
	proxy := &egressproxy.Proxy{
		Labs: labs, Resolver: resolver, Deny: deny, Stats: stats, Log: log.WithName("egress-proxy"),
		MaxConnAge:       time.Duration(cfg.EgressMaxConnSeconds) * time.Second,
		HostNewPerSecond: cfg.EgressHostNewPerSecond,
		LabMaxConns:      cfg.MaxConnectionsPerLab, LabNewPerSecond: cfg.NewConnectionsPerSecondPerLab,
	}
	dns := &egressproxy.DNS{Labs: labs, Resolver: resolver, Deny: deny, Stats: stats, Log: log.WithName("egress-dns"), QueriesPerSecond: cfg.EgressDNSQueriesPerSecond}

	ctx := context.Background()
	httpL, err := egressproxy.Listen(cfg.EgressHTTPPort)
	if err != nil {
		return err
	}
	tlsL, err := egressproxy.Listen(cfg.EgressTLSPort)
	if err != nil {
		return err
	}
	dnsTCP, err := egressproxy.Listen(cfg.EgressDNSPort)
	if err != nil {
		return err
	}
	dnsUDP, err := net.ListenPacket("udp4", ":"+strconv.Itoa(cfg.EgressDNSPort))
	if err != nil {
		return err
	}
	statsL, err := egressproxy.Listen(cfg.EgressStatsPort)
	if err != nil {
		return err
	}
	for name, f := range map[string]func() error{
		"web proxy (80)":  func() error { return proxy.Serve(ctx, httpL, false) },
		"web proxy (443)": func() error { return proxy.Serve(ctx, tlsL, true) },
		"dns (tcp)":       func() error { return dns.ServeTCP(ctx, dnsTCP) },
		"dns (udp)":       func() error { return dns.ServeUDP(ctx, dnsUDP) },
		"summaries": func() error {
			return (&http.Server{Handler: stats.Handler(), ReadHeaderTimeout: 5 * time.Second}).Serve(statsL)
		},
	} {
		go func() {
			if err := f(); err != nil {
				// the egress services are part of the gateway: when one stops, the pod must restart rather than leave the labs without a way out
				log.Error(err, "egress service stopped", "service", name)
				os.Exit(1)
			}
		}()
	}
	log.Info("egress services started", "http", cfg.EgressHTTPPort, "tls", cfg.EgressTLSPort, "dns", cfg.EgressDNSPort, "summaries", cfg.EgressStatsPort, "upstreams", upstreams)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
