//go:build linux

package proxywg

import (
	"github.com/cybericebox/laboratory/internal/errorlog"
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/bandwidth"
	"github.com/cybericebox/laboratory/internal/health"
	"github.com/cybericebox/laboratory/internal/names"
	proxy "github.com/cybericebox/laboratory/internal/proxy"
	"github.com/cybericebox/laboratory/internal/proxy/demux"
	"github.com/cybericebox/laboratory/internal/proxy/demux/fastpath"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(laboratoryv1alpha1.AddToScheme(scheme))
}

func Run() {
	journalLog, journal := errorlog.Setup("wg-demux", errorlog.Instance(), zap.New())
	ctrl.SetLogger(journalLog)
	log := ctrl.Log.WithName("proxy-wg")

	cfg, err := proxy.LoadWGConfig()
	if err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}

	fast := cfg.Mode != demux.ModeUserspace

	opts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: cfg.HealthAddr,
	}
	if fast {
		// The entry addresses come from this demux's Service and Node only (the LabGroups stay cluster-wide).
		opts.Cache = demux.CacheOptions(cfg.ServiceNamespace, cfg.ServiceName, cfg.NodeName)
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	table := demux.NewTable()
	limits := demux.Limits{
		MaxEntries: cfg.MaxEntries, PartialTTL: cfg.PartialTTL,
		GlobalHandshakeRate: cfg.GlobalHandshakeRate, GlobalHandshakeBurst: cfg.GlobalHandshakeBurst,
		MissRate: cfg.MissRate, MissBurst: cfg.MissBurst, RoamInterval: cfg.RoamInterval,
		SessionRate: cfg.SessionRate, SessionBurst: cfg.SessionBurst,
		Readers: cfg.Readers,
	}
	ct := demux.NewConnTrackWithLimits(limits)
	table.OnChange = ct.RemoveGroup

	watcher := &demux.LabGroupWatcher{
		Client:         mgr.GetClient(),
		Table:          table,
		VPNServicePort: names.WireGuardPort,
	}
	if err := watcher.SetupWithManager(mgr); err != nil {
		log.Error(err, "setup LabGroupWatcher")
		os.Exit(1)
	}

	// tc and xdp: the kernel forwards the transport packets of the sessions mirrored into it. A fast path that cannot start is
	// fatal (no silent fallback: the operator picks DEMUX_MODE=userspace by value).
	var fp *fastpath.FastPath
	if fast {
		fp, err = openFastPath(cfg)
		if err != nil {
			log.Error(err, "start the "+string(cfg.Mode)+" fast path (DEMUX_MODE=userspace runs without it)")
			os.Exit(1)
		}
		defer func() { _ = fp.Close() }()
		ct.SetSink(fp, fp)
		entries := &demux.EntryWatcher{
			Client: mgr.GetClient(), NodeName: cfg.NodeName, HostIP: cfg.HostIP,
			ServiceName: cfg.ServiceName, ServiceNamespace: cfg.ServiceNamespace,
			OnChange: func(addrs []netip.AddrPort) {
				if err := fp.SetEntries(addrs); err != nil {
					log.Error(err, "write the entry addresses")
					return
				}
				log.Info("entry addresses", "addresses", addrs)
			},
		}
		if err := entries.SetupWithManager(mgr); err != nil {
			log.Error(err, "setup EntryWatcher")
			os.Exit(1)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	stop := make(chan struct{})

	dmx, err := demux.NewWithLimits(cfg.ListenAddr, table, ct, limits)
	if err != nil {
		log.Error(err, "create demux")
		os.Exit(1)
	}

	// Ready means the demux really serves: the UDP socket is bound (above), the caches have synced, and every group has been put in the
	// table (a handshake of a group that is not in it is dropped, and the client re-handshakes after a long wait).
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		log.Error(err, "add health check")
		os.Exit(1)
	}
	checks := map[string]healthz.Checker{
		"caches": health.CacheSynced(mgr.GetCache()),
		"table":  health.Func("the demux table is not filled from the groups yet", watcher.Synced),
	}
	if fast {
		// Attached in front on every device, and the entry addresses known.
		checks["fastpath"] = func(*http.Request) error { return fp.Ready() }
	}
	for name, check := range checks {
		if err := mgr.AddReadyzCheck(name, check); err != nil {
			log.Error(err, "add ready check")
			os.Exit(1)
		}
	}

	go func() {
		<-ctx.Done()
		close(stop)
	}()
	var stats func() map[string]uint64
	// The bandwidth limits of this node come from the annotation of the pod (a downward API volume): the class and the ceiling of one
	// participant. The userspace relay throttles every session by them; the kernel fast path gets the matching packet rate. A pod made
	// without the volume has no limits.
	runs := []func(){func() {
		err := bandwidth.Watch(ctx, bandwidth.AnnotationsFile,
			func(l bandwidth.Limits) {
				ct.Throttle().SetLimits(l.ClassKbps, l.CapKbps)
				if fast {
					pps := demux.PacketRateFor(cfg.SessionRate, l)
					if err := fp.SetRate(pps, demux.PacketBurstFor(pps, cfg.SessionBurst)); err != nil {
						log.Error(err, "set the packet rate of the fast path")
					}
				}
				log.Info("bandwidth limits", "classKbps", l.ClassKbps, "participantKbps", l.CapKbps)
			},
			func(err error) { log.Error(err, "read the bandwidth annotation") })
		if err != nil {
			log.Error(err, "watch the bandwidth annotation")
		}
	}, func() { ct.RunTTLCleanup(stop) }, func() { table.RunResolverContext(ctx, 30*time.Second) }, func() { dmx.Run(stop) }}
	if fast {
		stats = func() map[string]uint64 { return fp.Stats() }
		runs = append(runs, func() { restoreOnce(ctx, mgr, watcher, fp, ct, table) }, func() { resync(ctx, cfg.Resync, fp, ct) })
	}
	runs = append(runs, func() { serveMetrics(ctx, cfg.MetricsAddr, demux.MetricsHandler(cfg.Mode, dmx, stats)) })
	var workers sync.WaitGroup
	for _, run := range runs {
		workers.Add(1)
		go func(run func()) { defer workers.Done(); run() }(run)
	}
	defer func() { cancel(); dmx.Close(); workers.Wait() }()

	log.Info("starting wg proxy", "udp", cfg.ListenAddr)
	errorlog.Start(ctx, journal, ctrl.GetConfigOrDie(), errorlog.Namespace("laboratory-system"), ctrl.Log.WithName("error-journal"))
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager error")
		os.Exit(1)
	}
}

func openFastPath(cfg *proxy.WGConfig) (*fastpath.FastPath, error) {
	_, p, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return nil, err
	}
	return fastpath.Open(fastpath.Options{
		Mode: cfg.Mode, PinPath: cfg.PinPath, HostIP: cfg.HostIP, SourcePort: uint16(port),
		MaxEntries: cfg.MaxEntries, TTL: demux.SessionTTL, Rate: cfg.SessionRate, Burst: cfg.SessionBurst,
	})
}

// restoreOnce takes the sessions of the previous demux back into the table as soon as the table knows every group.
func restoreOnce(ctx context.Context, mgr ctrl.Manager, watcher *demux.LabGroupWatcher, fp *fastpath.FastPath, ct *demux.ConnTrack, table *demux.Table) {
	log := ctrl.Log.WithName("proxy-wg")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if mgr.GetCache().WaitForCacheSync(ctx) && watcher.Synced(ctx) {
			restored, dropped, err := restoreSessions(fp, ct, table.Entries())
			if err != nil {
				log.Error(err, "restore the sessions of the fast path")
			} else {
				log.Info("restored the sessions of the fast path", "restored", restored, "dropped", dropped)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// resync brings the fast path's last-seen times into the table (so a session the kernel forwards does not expire there) and
// keeps our programs in front on every device.
func resync(ctx context.Context, every time.Duration, fp *fastpath.FastPath, ct *demux.ConnTrack) {
	log := ctrl.Log.WithName("proxy-wg")
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if seen, err := fp.LastSeen(); err != nil {
			log.Error(err, "read the last-seen times of the fast path")
		} else {
			ct.Seen(seen)
		}
		if err := fp.EnsureOrder(); err != nil {
			log.Error(err, "keep the fast path in front")
		}
	}
}

func serveMetrics(ctx context.Context, addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		ctrl.Log.WithName("proxy-wg").Error(err, "metrics server", "addr", addr)
	}
}
