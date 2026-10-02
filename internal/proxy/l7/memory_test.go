package l7

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// D-8: what one connection of the L7 proxy costs in memory, measured on a real proxy process (this test binary run again as the proxy:
// TestL7MemoryHelper) serving TLS, with N requests in flight to a backend that holds every response open: each one holds the client's TLS
// connection, the proxy's goroutines and buffers, and an upstream connection. The numbers behind proxy.l7.resources and maxConnections
// in the chart (DEPLOY.md, "Sizing the proxy").

const memHelperEnv = "L7_MEMORY_HELPER_BACKEND"

type memSample struct {
	HeapInuse  uint64
	StackInuse uint64
	Sys        uint64
}

func (m memSample) live() uint64 { return m.HeapInuse + m.StackInuse }

// TestL7MemoryHelper is the proxy process of TestL7MemoryPerConnection; it does nothing in a normal run.
func TestL7MemoryHelper(t *testing.T) {
	backend := os.Getenv(memHelperEnv)
	if backend == "" {
		t.Skip("helper of TestL7MemoryPerConnection")
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	meter := NewMeter("boot", time.Now())
	h := NewHandler(staticKeys("acme", "k1", pub), testSecret, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backend, nil }).
		WithAccounting(meter, func(task, groupID string) (string, bool) { return "lab", true })
	srv := httptest.NewUnstartedServer(h)
	// the production server limits (chart proxy.l7)
	srv.Config.ReadHeaderTimeout = 10 * time.Second
	srv.Config.ReadTimeout = 5 * time.Minute
	srv.Config.IdleTimeout = 2 * time.Minute
	srv.Config.MaxHeaderBytes = 65536
	srv.StartTLS()
	mem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime.GC()
		debug.FreeOSMemory()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		_ = json.NewEncoder(w).Encode(memSample{HeapInuse: ms.HeapInuse, StackInuse: ms.StackInuse, Sys: ms.Sys})
	}))
	fmt.Printf("L7ADDR=%s MEMADDR=%s\n", srv.Listener.Addr(), mem.Listener.Addr())
	_, _ = io.Copy(io.Discard, os.Stdin) // lives until the parent closes its end
}

func TestL7MemoryPerConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("load measurement")
	}
	// the backend answers with the headers and one byte, then holds the response open
	var inflight atomic.Int64
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		inflight.Add(1)
		<-release
	}))
	defer backend.Close()
	defer close(release)

	cmd := exec.Command(os.Args[0], "-test.run=^TestL7MemoryHelper$", "-test.v")
	cmd.Env = append(os.Environ(), memHelperEnv+"="+backend.URL)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()
	var l7addr, memaddr string
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "L7ADDR=") {
			fmt.Sscanf(line, "L7ADDR=%s MEMADDR=%s", &l7addr, &memaddr)
			break
		}
	}
	if l7addr == "" {
		t.Fatal("the proxy process did not start")
	}
	go io.Copy(io.Discard, stdout)

	sample := func() memSample {
		resp, err := http.Get("http://" + memaddr)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var s memSample
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	cookie := signCookie(t, jwtClaims{GroupID: "g1", Client: "p-user", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
	var clients []*http.Client
	var mu sync.Mutex
	var failures atomic.Int64
	open := func(n int) {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			if i%40 == 39 {
				time.Sleep(20 * time.Millisecond) // the listen backlog is short: do not open them all in one instant
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}}
				req, _ := http.NewRequest("GET", "https://"+l7addr+"/", nil)
				req.Host = "web-abc123.challenges.example.com"
				req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
				resp, err := c.Do(req)
				if err != nil {
					failures.Add(1)
					t.Logf("request: %v", err)
					return
				}
				mu.Lock()
				clients = append(clients, c)
				mu.Unlock()
				_ = resp // the body stays open: the request is in flight
			}()
		}
		wg.Wait()
	}
	waitInflight := func(n int64) {
		for deadline := time.Now().Add(30 * time.Second); inflight.Load() < n; {
			if time.Now().After(deadline) {
				t.Fatalf("only %d of %d requests reached the backend", inflight.Load(), n)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	open(1) // warm up
	waitInflight(1)
	base := sample()
	steps := []int{300, 1000, 2000}
	prev, prevN := base, 1
	var perConn float64
	for _, n := range steps {
		open(n - prevN)
		waitInflight(int64(n))
		time.Sleep(300 * time.Millisecond)
		s := sample()
		perConn = float64(s.live()-prev.live()) / float64(n-prevN)
		t.Logf("%4d in-flight requests: heap+stack %5.1f MiB (+%.1f KiB per connection), runtime total %5.1f MiB", n, float64(s.live())/(1<<20), perConn/1024, float64(s.Sys)/(1<<20))
		prev, prevN = s, n
	}
	baseMiB := float64(base.live()) / (1 << 20)
	t.Logf("idle proxy: %.1f MiB; one in-flight proxied request (TLS + HTTP + upstream): about %.0f KiB", baseMiB, perConn/1024)
	// A regression guard, not the sizing: DEPLOY.md and the chart use these numbers with headroom (see "Sizing the proxy").
	if perConn > 160*1024 {
		t.Errorf("an in-flight connection costs %.0f KiB, the chart's sizing assumes at most 160 KiB", perConn/1024)
	}
	for _, c := range clients {
		c.CloseIdleConnections()
	}
}
