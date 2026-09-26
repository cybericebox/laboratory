package vpn

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/cybericebox/laboratory/pkg/vpnprobe"
)

// ProbePort is reachable through the WireGuard interface, never through the
// public UDP service. Keep this in sync with the participant VPN status API.
const ProbePort = vpnprobe.Port

type probeServer struct {
	server   *http.Server
	listener net.Listener
	done     chan struct{}
}

// This page is deliberately static: the VPN server knows only tunnel clients,
// not participant names, events, or event themes. It uses the platform palette.
//
//go:embed probe_page.html
var probePageHTML string

func probeHandler(supportEmail string) http.Handler {
	pageTemplate, err := template.New("probe").Parse(probePageHTML)
	var page bytes.Buffer
	if err == nil {
		err = pageTemplate.Execute(&page, struct{ SupportEmail string }{SupportEmail: supportEmail})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err != nil {
			http.Error(w, "probe page unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'")
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(page.Bytes())
	})
}

func startProbe(subnet *net.IPNet, port int, supportEmail string) (*probeServer, error) {
	if subnet == nil {
		return nil, fmt.Errorf("VPN probe requires an IPv4 client subnet")
	}
	address, err := vpnprobe.GatewayIP(subnet.String())
	if err != nil {
		return nil, err
	}
	gwIP := net.ParseIP(address)
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: gwIP, Port: port})
	if err != nil {
		return nil, fmt.Errorf("listen on VPN gateway %s: %w", gwIP, err)
	}
	p := &probeServer{
		server:   &http.Server{Handler: probeHandler(supportEmail), ReadHeaderTimeout: 5 * time.Second},
		listener: listener,
		done:     make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		if serveErr := p.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("VPN probe stopped: %v", serveErr)
		}
	}()
	return p, nil
}

func (p *probeServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.server.Shutdown(ctx); err != nil {
		_ = p.server.Close()
	}
	<-p.done
}
