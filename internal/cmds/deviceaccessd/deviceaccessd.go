//go:build linux

// Package deviceaccessd is the device access service as a command of the node image (`/node device-access`): a DaemonSet on the lab
// nodes, the only component that holds the container runtime socket. See package internal/deviceaccess.
package deviceaccessd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/cybericebox/laboratory/internal/deviceaccess"
	"github.com/cybericebox/laboratory/internal/grant"
	"github.com/cybericebox/laboratory/pkg/config"
)

// Config comes from the environment the chart sets.
type Config struct {
	Listen        string `env:"LISTEN" envDefault:":8080"`
	MetricsListen string `env:"METRICS_LISTEN" envDefault:":9102"`
	// ContainerdSock and ContainerdNamespace: the runtime socket (mounted at the same path as on the host) and the namespace of the
	// kubelet's CRI. FifoDir is the host directory for the FIFOs of the execs, mounted at the same path as on the host.
	ContainerdSock      string `env:"CONTAINERD_SOCK" envDefault:"/run/k0s/containerd.sock"`
	ContainerdNamespace string `env:"CONTAINERD_NAMESPACE" envDefault:"k8s.io"`
	FifoDir             string `env:"FIFO_DIR" envDefault:"/run/containerd/fifo"`

	// KeyDir is the directory of the tenants' access public keys, one PEM file <tenant>.<key id> each: the ConfigMap the operator mirrors from
	// the access key Secrets, mounted as files (public data). A console grant is verified with them; the service holds no key of its own.
	// KeyReload is how often the directory is read again; GrantMaxTTL is the longest exp - iat of a grant the service accepts.
	KeyDir      string        `env:"KEY_DIR" envDefault:"/etc/device-access/access-keys"`
	KeyReload   time.Duration `env:"KEY_RELOAD" envDefault:"15s"`
	GrantMaxTTL time.Duration `env:"GRANT_MAX_TTL" envDefault:"8h"`

	// The checker socket (empty path: none). The lab checker of the node asks the service over it to run the probe in a device; its
	// protection is the file permissions (group CheckerGID) and the peer credentials (user CheckerUID, group CheckerGID).
	CheckerSocket        string `env:"CHECKER_SOCKET"`
	CheckerGID           int    `env:"CHECKER_GID" envDefault:"65532"`
	CheckerUID           int    `env:"CHECKER_UID" envDefault:"65532"`
	CheckerMaxSeconds    int    `env:"CHECKER_MAX_SECONDS" envDefault:"120"`
	CheckerMaxConcurrent int    `env:"CHECKER_MAX_CONCURRENT" envDefault:"8"`

	// Console lines (chart consoleCables.enabled; empty: none). CableSocket is cable.sock, the socket of the node-agent of this node, in a
	// node directory the two pods share (file mode 0660 for group CableGID and the peer credentials of user CablePeerUID and group CableGID).
	// CableListen is the listener of the cable stream from the device access pods of other nodes (the pod IP and consoleCables.port). The
	// stream is authenticated with an Ed25519 identity this process makes at start and keeps in memory; there is no key file.
	CableSocket  string `env:"CABLE_SOCKET"`
	CableGID     int    `env:"CABLE_GID" envDefault:"0"`
	CablePeerUID int    `env:"CABLE_PEER_UID" envDefault:"0"`
	CableListen  string `env:"CABLE_LISTEN"`
	// NodeName is the node of the pod (downward API): its node key signs the cable streams it dials.
	NodeName string `env:"NODE_NAME"`

	PerClient int           `env:"LIMIT_PER_CLIENT" envDefault:"3"`
	PerLab    int           `env:"LIMIT_PER_LAB" envDefault:"10"`
	PerNode   int           `env:"LIMIT_PER_NODE" envDefault:"200"`
	Idle      time.Duration `env:"IDLE_TIMEOUT" envDefault:"30m"`
	Lifetime  time.Duration `env:"MAX_LIFETIME" envDefault:"8h"`
}

// Health is the command of the kubelet's exec probes: it asks the local listener for /healthz.
func Health() {
	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	if listen[0] == ':' {
		listen = "127.0.0.1" + listen
	}
	// With the chart the listener is bound to the pod IP (LISTEN=<pod ip>:<port>), and the probe asks that address.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listen+deviceaccess.HealthPath, nil)
	if err != nil {
		os.Exit(1)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		os.Exit(1)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}

// Run starts the service.
func Run() {
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("device-access")
	var cfg Config
	if err := config.Load(&cfg); err != nil {
		log.Error(err, "load config")
		os.Exit(1)
	}
	if err := run(cfg); err != nil {
		log.Error(err, "device access service stopped")
		os.Exit(1)
	}
}

func run(cfg Config) error {
	log := ctrl.Log.WithName("device-access")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	rt, err := deviceaccess.DialContainerd(cfg.ContainerdSock, cfg.ContainerdNamespace, cfg.FifoDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.Close() }()
	if cfg.GrantMaxTTL <= 0 {
		return errors.New("GRANT_MAX_TTL must be positive")
	}
	keys := grant.NewDirKeys(cfg.KeyDir, cfg.KeyReload)
	execs, fifos, err := rt.Sweep(ctx)
	if err != nil {
		return fmt.Errorf("sweep: %w", err)
	}
	log.Info("start-up sweep", "execsRemoved", execs, "fifoEntriesRemoved", fifos)

	lim := deviceaccess.DefaultLimits()
	lim.PerClient, lim.PerLab, lim.PerNode = cfg.PerClient, cfg.PerLab, cfg.PerNode
	lim.Idle, lim.Lifetime = cfg.Idle, cfg.Lifetime
	lim.KeyRecheck = cfg.KeyReload
	// The identity of the cable stream: new for every process, never written anywhere (the node-agent publishes its public half).
	id, err := deviceaccess.NewIdentity()
	if err != nil {
		return err
	}
	// The cable port is the same on every device access pod (the chart's consoleCables.port): the port of this listener is the only port
	// a peer is dialled on.
	cablePort := 0
	if cfg.CableListen != "" {
		_, p, err := net.SplitHostPort(cfg.CableListen)
		if err == nil {
			cablePort, err = strconv.Atoi(p)
		}
		if err != nil || cablePort <= 0 || cablePort > 65535 {
			return fmt.Errorf("CABLE_LISTEN %q has no port", cfg.CableListen)
		}
	}
	svc := deviceaccess.New(deviceaccess.Config{Runtime: rt, Limits: lim, Log: log, Auth: deviceaccess.NewAuth(keys.Lookup, cfg.GrantMaxTTL),
		Identity: id, NodeName: cfg.NodeName, CablePort: cablePort})

	api := deviceaccess.NewHTTPServer(cfg.Listen, svc.Handler())
	metrics := deviceaccess.NewHTTPServer(cfg.MetricsListen, svc.MetricsHandler())
	errc := make(chan error, 5)
	servers := []*http.Server{api, metrics}
	if cfg.CheckerSocket != "" {
		ln, err := deviceaccess.ListenChecker(cfg.CheckerSocket, cfg.CheckerGID, cfg.CheckerUID)
		if err != nil {
			return fmt.Errorf("the checker socket: %w", err)
		}
		checker := deviceaccess.NewHTTPServer("", svc.CheckerHandler(deviceaccess.CheckerLimits{MaxSeconds: cfg.CheckerMaxSeconds, MaxConcurrent: cfg.CheckerMaxConcurrent}))
		servers = append(servers, checker)
		go func() {
			if err := checker.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
		log.Info("checker socket", "path", cfg.CheckerSocket)
	}
	if cfg.CableSocket != "" {
		ln, err := deviceaccess.ListenCable(cfg.CableSocket, cfg.CableGID, cfg.CablePeerUID)
		if err != nil {
			return fmt.Errorf("cable.sock: %w", err)
		}
		sock := deviceaccess.NewHTTPServer("", svc.CableSockHandler())
		servers = append(servers, sock)
		go func() {
			if err := sock.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
		log.Info("cable socket", "path", cfg.CableSocket)
	}
	listeners := []*http.Server{api, metrics}
	if cfg.CableListen != "" {
		if cfg.NodeName == "" {
			return errors.New("CABLE_LISTEN needs NODE_NAME")
		}
		cable := deviceaccess.NewHTTPServer(cfg.CableListen, svc.CableHandler())
		servers = append(servers, cable)
		listeners = append(listeners, cable)
		log.Info("cable stream listener", "listen", cfg.CableListen)
	}
	for _, srv := range listeners {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	log.Info("device access service started", "listen", cfg.Listen)
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	// Stop accepting, end the running streams and remove their execs before the process goes.
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(sctx)
	}
	return svc.Shutdown(sctx)
}
