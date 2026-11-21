//go:build unix

package probe

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture is a device's /proc and /etc.
func fixture(t *testing.T) view {
	t.Helper()
	root := t.TempDir()
	v := view{proc: filepath.Join(root, "proc"), etc: filepath.Join(root, "etc"), self: 999}
	write(t, filepath.Join(v.etc, "passwd"), "root:x:0:0:root:/root:/bin/sh\nwww-data:x:33:33::/var/www:/bin/false\nnobody:x:65534:65534::/:/bin/false\n")
	write(t, filepath.Join(v.etc, "group"), "root:x:0:\nwww-data:x:33:\nstaff:x:50:www-data\nops:x:60:root,www-data\n")
	return v
}

func addProc(t *testing.T, v view, pid int, comm, cmdline string, uid int) {
	t.Helper()
	d := filepath.Join(v.proc, strconv.Itoa(pid))
	write(t, filepath.Join(d, "comm"), comm+"\n")
	write(t, filepath.Join(d, "cmdline"), cmdline)
	write(t, filepath.Join(d, "status"), "Name:\t"+comm+"\nUid:\t"+strconv.Itoa(uid)+"\t"+strconv.Itoa(uid)+"\t0\t0\n")
}

func TestProcessTable(t *testing.T) {
	v := fixture(t)
	addProc(t, v, 1, "nginx", "nginx: master process /usr/sbin/nginx\x00-g\x00daemon off;\x00", 0)
	addProc(t, v, 7, "nginx", "nginx: worker process\x00", 33)
	addProc(t, v, 8, "kworker/0:1", "", 0)
	addProc(t, v, 9, "postgresql-16-ma", "/usr/lib/postgresql/16/bin/postgres\x00-D\x00/var/lib\x00", 33)
	addProc(t, v, 999, "probe", "/.cib/probe/probe\x00check-observe\x00", 0) // the probe itself
	if err := os.MkdirAll(filepath.Join(v.proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	get := func() ProcessFacts {
		got := v.observe(ObserveRequest{Kind: KindProcess})
		if got.Error != "" || got.Processes == nil {
			t.Fatalf("%+v", got)
		}
		return *got.Processes
	}
	f := get()
	if len(f.List) != 4 || f.Truncated || f.Incomplete != 0 {
		t.Fatalf("got %+v", f)
	}
	by := map[int]Process{}
	for _, p := range f.List {
		by[p.PID] = p
	}
	if p := by[1]; p.Comm != "nginx" || p.Arg0 != "nginx" || p.User != "root" || p.Cut || !strings.Contains(p.Cmdline, "nginx: master process /usr/sbin/nginx -g daemon off;") {
		t.Errorf("pid 1: %+v", p)
	}
	if p := by[7]; p.User != "www-data" || p.UID != 33 {
		t.Errorf("pid 7: %+v", p)
	}
	if p := by[8]; p.Arg0 != "" || p.Comm != "kworker/0:1" {
		t.Errorf("kernel thread: %+v", p)
	}
	if p := by[9]; p.Arg0 != "postgres" {
		t.Errorf("pid 9: %+v", p)
	}
	if _, ok := by[999]; ok {
		t.Error("the probe listed itself")
	}
	// A long command line is cut where it is returned and says so; its first word is still known.
	write(t, filepath.Join(v.proc, "9", "cmdline"), "/usr/bin/postgres\x00"+strings.Repeat("a", MaxCmdlineBytes+100))
	for _, p := range get().List {
		if p.PID == 9 && (!p.Cut || len(p.Cmdline) > MaxCmdlineBytes || p.Arg0 != "postgres") {
			t.Errorf("a long command line: %d bytes, cut %v, arg0 %q", len(p.Cmdline), p.Cut, p.Arg0)
		}
	}
	// Something that vanished half-way is gone; something that cannot be read is counted.
	if err := os.Remove(filepath.Join(v.proc, "9", "status")); err != nil {
		t.Fatal(err)
	}
	if f := get(); len(f.List) != 3 || f.Incomplete != 0 {
		t.Errorf("a process that vanished: %+v", f)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(filepath.Join(v.proc, "8", "comm"), 0); err != nil {
			t.Fatal(err)
		}
		if f := get(); f.Incomplete != 1 || len(f.List) != 2 {
			t.Errorf("an unreadable process: %+v", f)
		}
	}
}

func TestProcessTableIsBounded(t *testing.T) {
	v := fixture(t)
	for i := 1; i <= MaxProcesses+5; i++ {
		addProc(t, v, i, "p", "p\x00", 0)
	}
	got := v.observe(ObserveRequest{Kind: KindProcess})
	if got.Processes == nil || len(got.Processes.List) != MaxProcesses || !got.Processes.Truncated {
		t.Fatalf("got %d, truncated %v", len(got.Processes.List), got.Processes.Truncated)
	}
}

func TestParseHexAddr(t *testing.T) {
	for _, c := range []struct {
		in   string
		addr string
		port int
		ok   bool
	}{
		{"0100007F:1F90", "127.0.0.1", 8080, true},
		{"00000000:0050", "0.0.0.0", 80, true},
		{"0F02000A:01BB", "10.0.2.15", 443, true},
		{"00000000000000000000000001000000:0016", "::1", 22, true},
		{"00000000000000000000000000000000:1F90", "::", 8080, true},
		{"0100007F", "", 0, false},
		{"zz:10", "", 0, false},
		{"0100007F:ZZZZ", "", 0, false},
	} {
		addr, port, ok := parseHexAddr(c.in)
		if ok != c.ok || addr != c.addr || port != c.port {
			t.Errorf("%s: got %q %d %v", c.in, addr, port, ok)
		}
	}
}

const tcpTable = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 5555 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 5556 1 0000000000000000 100 0 0 10 0
   2: 0100007F:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 5557 1 0000000000000000 100 0 0 10 0
   3: 0F02000A:1F90 0100007F:D431 01 00000000:00000000 00:00000000 00000000     0        0 5558 1 0000000000000000 100 0 0 10 0
`

func TestParseNetTable(t *testing.T) {
	got, bad := parseNetTable(tcpTable, "tcp", 8080)
	if bad != 0 {
		t.Fatalf("bad lines %d", bad)
	}
	if len(got) != 2 || got[0].addr != "0.0.0.0" || got[0].inode != 5555 || got[1].addr != "127.0.0.1" {
		t.Fatalf("only LISTEN sockets on the port: %+v", got)
	}
	udp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops\n" +
		"   0: 00000000:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 7001 2 0000000000000000 0\n" +
		"   1: 00000000:0036 0100007F:0035 01 00000000:00000000 00:00000000 00000000     0        0 7002 2 0000000000000000 0\n"
	if got, _ := parseNetTable(udp, "udp", 53); len(got) != 1 || got[0].inode != 7001 {
		t.Fatalf("udp: %+v", got)
	}
	if got, _ := parseNetTable(udp, "udp", 54); len(got) != 0 {
		t.Fatalf("a connected udp socket does not wait: %+v", got)
	}
}

// A line of the socket table that cannot be read could be the socket that is asked about: the question is not answered.
func TestPortTableWithAnUnreadableLine(t *testing.T) {
	v := fixture(t)
	write(t, filepath.Join(v.proc, "net", "tcp"), tcpTable+"   9: garbage\n")
	if got := v.observe(ObserveRequest{Kind: KindPort, Port: 8080, Protocol: "tcp"}); got.Error == "" || got.Port != nil {
		t.Fatalf("got %+v", got)
	}
	write(t, filepath.Join(v.proc, "net", "tcp"), tcpTable+"   9: ZZZZ:1F90 00000000:0000 0A 0:0 0:0 0 0 0 5559 1\n")
	if got := v.observe(ObserveRequest{Kind: KindPort, Port: 8080, Protocol: "tcp"}); got.Error == "" {
		t.Fatalf("an address that cannot be read: %+v", got)
	}
}

func TestPortOwnersNotAllowedToBeSeen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	v := fixture(t)
	addProc(t, v, 1, "nginx", "nginx\x00", 0)
	write(t, filepath.Join(v.proc, "net", "tcp"), tcpTable)
	fdDir := filepath.Join(v.proc, "1", "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fdDir, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(fdDir, 0o755) }()
	got := v.observe(ObserveRequest{Kind: KindPort, Port: 8080, Protocol: "tcp"})
	if got.Port == nil || !got.Port.OwnersIncomplete {
		t.Fatalf("got %+v", got)
	}
}

func TestPortWithOwners(t *testing.T) {
	v := fixture(t)
	addProc(t, v, 1, "nginx", "nginx: master\x00", 0)
	addProc(t, v, 2, "sleep", "sleep\x00", 0)
	write(t, filepath.Join(v.proc, "net", "tcp"), tcpTable)
	fdDir := filepath.Join(v.proc, "1", "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[5555]", filepath.Join(fdDir, "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(fdDir, "0")); err != nil {
		t.Fatal(err)
	}
	got := v.observe(ObserveRequest{Kind: KindPort, Port: 8080, Protocol: "tcp"})
	if got.Error != "" || got.Port == nil || len(got.Port.Listeners) != 2 {
		t.Fatalf("got %+v", got)
	}
	var wild, loop Listener
	for _, l := range got.Port.Listeners {
		if l.Addr == "0.0.0.0" {
			wild = l
		} else {
			loop = l
		}
	}
	if len(wild.Procs) != 1 || wild.Procs[0] != "nginx" || loop.Addr != "127.0.0.1" || len(loop.Procs) != 0 {
		t.Fatalf("owners: wildcard %+v, loopback %+v", wild, loop)
	}
	if got := v.observe(ObserveRequest{Kind: KindPort, Port: 9999, Protocol: "tcp"}); got.Port == nil || len(got.Port.Listeners) != 0 {
		t.Fatalf("nothing listens: %+v", got)
	}
	if got := v.observe(ObserveRequest{Kind: KindPort, Port: 0, Protocol: "tcp"}); got.Error == "" {
		t.Fatal("port 0 must be refused")
	}
	if got := v.observe(ObserveRequest{Kind: KindPort, Port: 80, Protocol: "sctp"}); got.Error == "" {
		t.Fatal("sctp must be refused")
	}
}

func TestKernelParam(t *testing.T) {
	v := fixture(t)
	write(t, filepath.Join(v.proc, "sys", "net", "ipv4", "ip_forward"), "1\n")
	write(t, filepath.Join(v.proc, "sys", "kernel", "hostname"), "lab-box \n")
	if got := v.observe(ObserveRequest{Kind: KindKernelParam, Name: "net.ipv4.ip_forward"}); got.KernelParam == nil || !got.KernelParam.Exists || got.KernelParam.Value != "1" {
		t.Fatalf("got %+v", got)
	}
	if got := v.observe(ObserveRequest{Kind: KindKernelParam, Name: "kernel.hostname"}); got.KernelParam.Value != "lab-box" {
		t.Fatalf("value is trimmed: %+v", got.KernelParam)
	}
	if got := v.observe(ObserveRequest{Kind: KindKernelParam, Name: "net.ipv4.nope"}); got.KernelParam == nil || got.KernelParam.Exists {
		t.Fatalf("missing: %+v", got)
	}
	if got := v.observe(ObserveRequest{Kind: KindKernelParam, Name: "net.ipv4"}); got.Error == "" {
		t.Fatalf("a directory is not a value: %+v", got)
	}
	for _, bad := range []string{"", "..", "net..ipv4", "net/ipv4/ip_forward", "../../etc/passwd", "NET.ipv4", ".net", strings.Repeat("a", 300)} {
		if got := v.observe(ObserveRequest{Kind: KindKernelParam, Name: bad}); got.Error == "" {
			t.Errorf("%q must be refused", bad)
		}
	}
	write(t, filepath.Join(v.proc, "sys", "kernel", "big"), strings.Repeat("a", MaxParamBytes+1))
	if got := v.observe(ObserveRequest{Kind: KindKernelParam, Name: "kernel.big"}); got.Error == "" {
		t.Fatal("a value over the bound is an error")
	}
}

func TestDirectory(t *testing.T) {
	v := fixture(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	hashes := func(f *DirectoryFacts) map[string]bool {
		m := map[string]bool{}
		for _, h := range f.Hashes {
			m[string(h)] = true
		}
		return m
	}
	dir := t.TempDir()
	for _, n := range []string{"config.php", "uploads", ".hidden"} {
		write(t, filepath.Join(dir, n), "x")
	}
	if err := os.Symlink("/nowhere", filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	got := v.observe(ObserveRequest{Kind: KindDirectory, Path: dir, Key: key})
	if got.Error != "" || got.Directory == nil || !got.Directory.Exists || !got.Directory.IsDir || got.Directory.Mode != 0o750 || got.Directory.Truncated || len(got.Directory.Hashes) != 4 {
		t.Fatalf("got %+v %v", got.Directory, got.Error)
	}
	have := hashes(got.Directory)
	for _, n := range []string{"config.php", "uploads", ".hidden", "dangling"} {
		if !have[string(EntryHash(key, n))] {
			t.Errorf("%s is missing from the hashes", n)
		}
	}
	if have[string(EntryHash(key, "install.php"))] {
		t.Error("a name that is not there")
	}
	if len(got.Directory.Hashes[0]) != DirHashBytes {
		t.Errorf("hash length %d", len(got.Directory.Hashes[0]))
	}
	// A hash means something only with its key.
	if hashes(got.Directory)[string(EntryHash([]byte("another key another key another"), "config.php"))] {
		t.Error("a hash under another key matched")
	}
	if got.Directory.UID != uint32(os.Getuid()) {
		t.Errorf("uid %d", got.Directory.UID)
	}
	if got := v.observe(ObserveRequest{Kind: KindDirectory, Path: filepath.Join(dir, "nope"), Key: key}); got.Directory == nil || got.Directory.Exists || got.Directory.Hashes != nil {
		t.Fatalf("missing: %+v", got)
	}
	if got := v.observe(ObserveRequest{Kind: KindDirectory, Path: filepath.Join(dir, "config.php"), Key: key}); got.Directory == nil || !got.Directory.Exists || got.Directory.IsDir {
		t.Fatalf("a file is not a directory: %+v", got.Directory)
	}
	if got := v.observe(ObserveRequest{Kind: KindDirectory, Path: "relative", Key: key}); got.Error == "" {
		t.Fatal("relative path")
	}
	if got := v.observe(ObserveRequest{Kind: KindDirectory, Path: dir}); got.Error == "" {
		t.Fatal("no key")
	}
	if got := v.observe(ObserveRequest{Kind: KindDirectory, Path: dir, Key: []byte("short")}); got.Error == "" {
		t.Fatal("a short key")
	}
	// A directory over the bound says so, and returns only the bound.
	big := t.TempDir()
	for i := 0; i < MaxDirHashes+7; i++ {
		write(t, filepath.Join(big, "f"+strconv.Itoa(i)), "")
	}
	got = v.observe(ObserveRequest{Kind: KindDirectory, Path: big, Key: key})
	if got.Directory == nil || len(got.Directory.Hashes) != MaxDirHashes || !got.Directory.Truncated {
		t.Fatalf("bound: %d hashes, truncated %v", len(got.Directory.Hashes), got.Directory.Truncated)
	}
	// A directory that cannot be read is an error, not an empty listing.
	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		write(t, filepath.Join(locked, "inner"), "x")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(locked, 0o700) }()
		if got := v.observe(ObserveRequest{Kind: KindDirectory, Path: locked, Key: key}); got.Error == "" {
			t.Errorf("an unreadable directory is not an empty one: %+v", got)
		}
	}
}

func TestRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"state": "up"}`)) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", MaxRequestBody+10)))
	})
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://elsewhere.invalid/", http.StatusFound)
	})
	mux.HandleFunc("/head", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Method", r.Method) })
	mux.HandleFunc("/ua", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.Header.Get("User-Agent"))) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tls := httptest.NewTLSServer(mux)
	defer tls.Close()
	hostPort := func(s *httptest.Server) (string, int) {
		h, p, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(s.URL, "http://"), "https://"))
		n, _ := strconv.Atoi(p)
		return h, n
	}
	host, port := hostPort(srv)
	thost, tport := hostPort(tls)
	do := func(r ObserveRequest) *RequestFacts {
		r.Kind = KindRequest
		if r.TimeoutMs == 0 {
			r.TimeoutMs = 3000
		}
		got := Observe(r)
		if got.Error != "" || got.Request == nil {
			t.Fatalf("%+v: %+v", r, got)
		}
		return got.Request
	}
	if f := do(ObserveRequest{Protocol: "tcp", Host: host, Port: port}); !f.Connected {
		t.Errorf("tcp: %+v", f)
	}
	if f := do(ObserveRequest{Protocol: "http", Host: host, Port: port, URLPath: "/ok"}); !f.Connected || f.Status != 200 || string(f.Body) != `{"state": "up"}` {
		t.Errorf("http: %+v", f)
	}
	if f := do(ObserveRequest{Protocol: "http", Host: host, Port: port, URLPath: "/missing"}); f.Status != 404 {
		t.Errorf("404 is an answer: %+v", f)
	}
	if f := do(ObserveRequest{Protocol: "https", Host: thost, Port: tport, URLPath: "/ok"}); !f.Connected || f.Status != 200 {
		t.Errorf("https with a certificate nobody trusts: %+v", f)
	}
	if f := do(ObserveRequest{Protocol: "http", Host: host, Port: port, URLPath: "/moved"}); f.Status != 302 {
		t.Errorf("a redirect is not followed: %+v", f)
	}
	if f := do(ObserveRequest{Protocol: "http", Host: host, Port: port, URLPath: "/big"}); len(f.Body) != MaxRequestBody || !f.Truncated {
		t.Errorf("the body is cut: %d %v", len(f.Body), f.Truncated)
	}
	if f := do(ObserveRequest{Protocol: "http", Host: host, Port: port, URLPath: "/ua", Method: "HEAD"}); f.Status != 200 || len(f.Body) != 0 {
		t.Errorf("head: %+v", f)
	}
	if f := do(ObserveRequest{Protocol: "http", Host: host, Port: port, URLPath: "/ua"}); string(f.Body) != "cybericebox-check" {
		t.Errorf("user agent: %q", f.Body)
	}
	// Nothing listens: refused. An https question to a plain http port fails in tls.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	if f := do(ObserveRequest{Protocol: "tcp", Host: "127.0.0.1", Port: closedPort}); f.Connected || f.Failure != "refused" {
		t.Errorf("refused: %+v", f)
	}
	if f := do(ObserveRequest{Protocol: "https", Host: host, Port: port}); f.Connected || f.Failure == "" {
		t.Errorf("tls to a plain port: %+v", f)
	}
	// The whole request is bounded by its time.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	sh, sp := hostPort(slow)
	start := time.Now()
	f := do(ObserveRequest{Protocol: "http", Host: sh, Port: sp, TimeoutMs: 300})
	if f.Connected || f.Failure != "timeout" || time.Since(start) > 1500*time.Millisecond {
		t.Errorf("timeout: %+v after %v", f, time.Since(start))
	}
	for _, bad := range []ObserveRequest{
		{Kind: KindRequest, Protocol: "http", Host: "", Port: 80},
		{Kind: KindRequest, Protocol: "http", Host: "x", Port: 0},
		{Kind: KindRequest, Protocol: "ftp", Host: "x", Port: 21},
		{Kind: KindRequest, Protocol: "http", Host: "x", Port: 80, Method: "POST"},
		{Kind: KindRequest, Protocol: "http", Host: "x", Port: 80, URLPath: "no-slash"},
	} {
		if got := Observe(bad); got.Error == "" {
			t.Errorf("%+v must be refused", bad)
		}
	}
}

func TestPermissionFacts(t *testing.T) {
	dir := t.TempDir()
	rw := filepath.Join(dir, "rw")
	ro := filepath.Join(dir, "ro")
	x := filepath.Join(dir, "x")
	none := filepath.Join(dir, "none")
	for p, mode := range map[string]os.FileMode{rw: 0o600, ro: 0o400, x: 0o700, none: 0o000} {
		write(t, p, "x")
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	if os.Geteuid() == 0 {
		t.Skip("root passes every access test")
	}
	for _, c := range []struct {
		path    string
		r, w, x bool
	}{{rw, true, true, false}, {ro, true, false, false}, {x, true, true, true}, {none, false, false, false}} {
		got := permFacts(c.path)
		if !got.Exists || got.Read != c.r || got.Write != c.w || got.Execute != c.x {
			t.Errorf("%s: %+v", c.path, got)
		}
	}
	if got := permFacts(filepath.Join(dir, "nope")); got.Exists || got.Read || got.Write || got.Execute || got.Error != "" {
		t.Errorf("missing: %+v", got)
	}
	// A failure that is not the kernel's refusal is an error, never a "no": a loop of links, for one.
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if got := permFacts(loop); got.Error == "" || got.Read || got.Write {
		t.Errorf("a symlink loop: %+v", got)
	}
	// Nothing was written: the files are as they were.
	if raw, _ := os.ReadFile(rw); string(raw) != "x" {
		t.Error("a permission test changed a file")
	}
	// A directory the user may not search hides what is in it.
	locked := filepath.Join(dir, "locked")
	write(t, filepath.Join(locked, "inner"), "x")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(locked, 0o700) }()
	if got := permFacts(filepath.Join(locked, "inner")); got.Read || got.Write {
		t.Errorf("no search permission: %+v", got)
	}
}

func TestCredentialOf(t *testing.T) {
	v := fixture(t)
	c, ok := v.credentialOf("www-data")
	if !ok || c.Uid != 33 || c.Gid != 33 || len(c.Groups) != 3 || c.Groups[0] != 33 {
		t.Fatalf("got %+v %v", c, ok)
	}
	have := map[uint32]bool{}
	for _, g := range c.Groups {
		have[g] = true
	}
	if !have[50] || !have[60] || !have[33] {
		t.Errorf("groups %v: the primary group and every group that lists the user", c.Groups)
	}
	if c, ok := v.credentialOf("33"); !ok || c.Uid != 33 {
		t.Errorf("by number: %+v %v", c, ok)
	}
	if c, ok := v.credentialOf("4242"); !ok || c.Uid != 4242 || len(c.Groups) != 1 || c.Groups[0] != 4242 {
		t.Errorf("a number with no passwd line: %+v %v", c, ok)
	}
	if c, ok := v.credentialOf("nobody"); !ok || c.Uid != 65534 {
		t.Errorf("nobody: %+v %v", c, ok)
	}
	if _, ok := v.credentialOf("ghost"); ok {
		t.Error("no such user")
	}
	if got := v.observe(ObserveRequest{Kind: KindPermission, User: "ghost", Path: "/etc/passwd"}); got.Permission == nil || got.Permission.UserFound {
		t.Errorf("unknown user: %+v", got)
	}
}

func TestObserveUnknownKind(t *testing.T) {
	if got := Observe(ObserveRequest{Kind: "shell"}); got.Error == "" {
		t.Fatal("an unknown kind must be refused")
	}
}
