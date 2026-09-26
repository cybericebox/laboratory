package vpn

import (
	"context"
	"fmt"
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
const probePageHTML = `<!doctype html>
<html lang="uk">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Перевірка підключення</title>
  <style>
    :root{--brand:#211A52;color-scheme:light;--paper:#F7F7F9;--surface:#FFFFFF;--line:#E4E4EC;--ink:#16152B;--body:#2C2B42;--dim:#5A5A70;--soft:#F0F0F5;--action:var(--brand);--on-action:#FFFFFF}
    :root[data-theme="dark"]{color-scheme:dark;--paper:#383645;--surface:#413F4E;--line:#53515F;--ink:#EEEDF6;--body:#DAD9E5;--dim:#BBB9CB;--soft:#4A4858;--action:#E6E6EE;--on-action:var(--brand)}
    *{box-sizing:border-box}
    body{margin:0;min-height:100vh;background:var(--paper);color:var(--body);font:16px/1.55 system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
    .top{height:8px;background:var(--brand)}
    .frame{max-width:960px;margin:0 auto;padding:28px 24px 64px}
    header{display:flex;align-items:center;justify-content:space-between;gap:16px}
    .eyebrow{color:var(--dim);font-size:12px;font-weight:700;letter-spacing:.09em;text-transform:uppercase}
    button{border:1px solid var(--line);border-radius:9px;background:var(--surface);color:var(--ink);padding:9px 14px;font:inherit;font-size:14px;cursor:pointer}
    button:hover{background:var(--soft)}button:focus-visible{outline:3px solid var(--action);outline-offset:3px}
    main{max-width:720px;margin:105px auto 0;padding:46px 50px 44px;border:1px solid var(--line);border-radius:16px;background:var(--surface)}
    .mark{display:grid;place-items:center;width:50px;height:50px;border-radius:12px;background:var(--brand);color:#FFFFFF;font-size:30px;font-weight:700}
    .label{margin:25px 0 8px;color:var(--dim);font-size:13px;font-weight:700;letter-spacing:.04em;text-transform:uppercase}
    h1{margin:0;color:var(--ink);font-size:clamp(28px,4vw,39px);line-height:1.2;letter-spacing:-.035em}
    .intro{margin:18px 0 0;font-size:18px}
    .note{margin:34px 0 0;padding:18px 20px;border-left:4px solid var(--brand);border-radius:0 8px 8px 0;background:var(--soft);color:var(--body)}
    .foot{max-width:720px;margin:18px auto 0;color:var(--dim);font-size:13px;text-align:center}
    @media(max-width:640px){.frame{padding:22px 16px 40px}main{margin-top:72px;padding:32px 25px}.intro{font-size:16px}}
  </style>
</head>
<body>
  <div class="top" aria-hidden="true"></div>
  <div class="frame">
    <header><span class="eyebrow">Перевірка підключення</span><button id="theme" type="button" aria-label="Змінити тему">Темна тема</button></header>
    <main>
      <div class="mark" aria-hidden="true">✓</div>
      <p class="label">Тунель доступний</p>
      <h1>Вітаємо! Підключення працює</h1>
      <p class="intro">Ви підключилися до своєї групи лабораторій.</p>
      <p class="note">Доступ до окремих завдань відкривається окремо.</p>
    </main>
    <p class="foot">Ця сторінка доступна лише через VPN-тунель.</p>
  </div>
  <script>
    (function(){
      var root=document.documentElement,button=document.getElementById('theme'),saved='';
      try{saved=localStorage.getItem('lab-probe-theme')||''}catch(_){}
      var dark=saved? saved==='dark':window.matchMedia('(prefers-color-scheme: dark)').matches;
      function apply(){root.dataset.theme=dark?'dark':'light';button.textContent=dark?'Світла тема':'Темна тема';button.setAttribute('aria-pressed',String(dark))}
      button.addEventListener('click',function(){dark=!dark;try{localStorage.setItem('lab-probe-theme',dark?'dark':'light')}catch(_){}apply()});
      apply();
    })();
  </script>
</body>
</html>`

func probeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'")
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(probePageHTML))
	})
}

func startProbe(subnet *net.IPNet, port int) (*probeServer, error) {
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
		server:   &http.Server{Handler: probeHandler(), ReadHeaderTimeout: 5 * time.Second},
		listener: listener,
		done:     make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		if serveErr := p.server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
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
