package vpn

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProbeHandlerShowsOnlyConnectionResult(t *testing.T) {
	r := httptest.NewRecorder()
	probeHandler("help@example.org").ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/", nil))
	if r.Code != http.StatusOK {
		t.Fatalf("status = %d", r.Code)
	}
	if contentType := r.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if !strings.Contains(r.Body.String(), "<h1>") ||
		!strings.Contains(r.Body.String(), "Вітаємо!</h1>") ||
		!strings.Contains(r.Body.String(), "Ви успішно підключилися до групи лабораторій.") ||
		!strings.Contains(r.Body.String(), "Доступ до окремих лабораторій (завдань) відкривається окремо.") {
		t.Fatalf("unexpected body: %q", r.Body.String())
	}
	if strings.Count(r.Body.String(), "#211A52") != 1 ||
		!strings.Contains(r.Body.String(), "--brand:#211A52") ||
		!strings.Contains(r.Body.String(), "Системна") ||
		!strings.Contains(r.Body.String(), "Світла") ||
		!strings.Contains(r.Body.String(), "Темна") {
		t.Fatalf("fixed platform palette or theme switcher missing")
	}
	if strings.Contains(r.Body.String(), "event") || strings.Contains(r.Body.String(), "team") {
		t.Fatalf("probe leaks event data: %q", r.Body.String())
	}
	if r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", r.Header().Get("Cache-Control"))
	}
}

func TestProbeHandlerRendersSupportEmail(t *testing.T) {
	r := httptest.NewRecorder()
	probeHandler("help@example.org").ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(r.Body.String(), `href="mailto:help@example.org"`) ||
		!strings.Contains(r.Body.String(), ">help@example.org</a>") {
		t.Fatalf("support address missing: %q", r.Body.String())
	}
}

func TestProbeBindsOnlyWireGuardGateway(t *testing.T) {
	_, subnet, err := net.ParseCIDR("127.0.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := startProbe(subnet, 0, "help@example.org")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	addr := probe.listener.Addr().(*net.TCPAddr)
	if !addr.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("bound to %s", addr.IP)
	}
	response, err := http.Get("http://" + addr.String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	other := net.JoinHostPort("127.0.0.2", strconv.Itoa(addr.Port))
	connection, err := net.DialTimeout("tcp", other, 250*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatalf("probe unexpectedly reachable on %s", other)
	}
}
