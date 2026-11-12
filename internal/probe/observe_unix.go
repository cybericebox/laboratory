//go:build unix

package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// view is where the probe reads the device from. In a device it is "/proc" and "/etc"; a test points it at a fixture.
type view struct {
	proc string
	etc  string
	self int
}

func realView() view { return view{proc: "/proc", etc: "/etc", self: os.Getpid()} }

// Observe answers one question (the entry of check-observe).
func Observe(req ObserveRequest) ObserveResponse { return realView().observe(req) }

func (v view) observe(req ObserveRequest) ObserveResponse {
	switch req.Kind {
	case KindDirectory:
		return v.directory(req)
	case KindProcess:
		return v.processes()
	case KindPort:
		return v.port(req)
	case KindKernelParam:
		return v.kernelParam(req)
	case KindRequest:
		return makeRequest(req)
	case KindPermission:
		return v.permission(req)
	}
	return ObserveResponse{Error: "unknown observation"}
}

func (v view) idNames() IDNames {
	return ParseIDNames(readBounded(filepath.Join(v.etc, "passwd")), readBounded(filepath.Join(v.etc, "group")))
}

func notFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// ---- directory ---------------------------------------------------------------------------------------------------------

func (v view) directory(req ObserveRequest) ObserveResponse {
	if req.Path == "" || req.Path[0] != '/' {
		return ObserveResponse{Error: "the path is not absolute"}
	}
	st, err := os.Stat(req.Path)
	switch {
	case notFound(err):
		return ObserveResponse{Directory: &DirectoryFacts{}}
	case err != nil:
		return ObserveResponse{Error: "cannot look at the directory: " + errnoName(err)}
	}
	f := &DirectoryFacts{Exists: true, IsDir: st.IsDir(), Mode: modeBits(st.Mode())}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		f.UID, f.GID = sys.Uid, sys.Gid
	}
	names := v.idNames()
	f.User, f.Group = names.Users[f.UID], names.Groups[f.GID]
	if !f.IsDir {
		return ObserveResponse{Directory: f}
	}
	if len(req.Key) < MinDirKeyBytes {
		return ObserveResponse{Error: "the question has no key"}
	}
	d, err := os.Open(req.Path)
	if err != nil {
		return ObserveResponse{Error: "cannot read the directory: " + errnoName(err)}
	}
	defer func() { _ = d.Close() }()
	for {
		names, err := d.Readdirnames(1024)
		for _, n := range names {
			if len(f.Hashes) >= MaxDirHashes {
				f.Truncated = true
				return ObserveResponse{Directory: f}
			}
			f.Hashes = append(f.Hashes, EntryHash(req.Key, n))
		}
		if errors.Is(err, io.EOF) {
			return ObserveResponse{Directory: f}
		}
		if err != nil {
			return ObserveResponse{Error: "cannot read the directory: " + errnoName(err)}
		}
	}
}

// modeBits are the permission bits with setuid, setgid and sticky, as a number.
func modeBits(m fs.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// ---- process -----------------------------------------------------------------------------------------------------------

func (v view) processes() ObserveResponse {
	ents, err := os.ReadDir(v.proc)
	if err != nil {
		return ObserveResponse{Error: "cannot read the process table: " + errnoName(err)}
	}
	names := v.idNames()
	facts := &ProcessFacts{List: []Process{}}
	for i, e := range ents {
		if i >= MaxProcessScan {
			facts.Incomplete++
			break
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == v.self {
			continue
		}
		p, state := v.process(pid, names)
		switch state {
		case procGone:
			continue
		case procUnreadable:
			facts.Incomplete++
			continue
		}
		if len(facts.List) >= MaxProcesses {
			facts.Truncated = true
			break
		}
		facts.List = append(facts.List, p)
	}
	return ObserveResponse{Processes: facts}
}

type procState int

const (
	procOK procState = iota
	procGone
	procUnreadable
)

// process reads one process. procGone: it ended while we looked. procUnreadable: it is there but not readable (hidepid, a denied
// read). procCut: its command line is longer than MaxCmdlineBytes.
func (v view) process(pid int, names IDNames) (Process, procState) {
	dir := filepath.Join(v.proc, strconv.Itoa(pid))
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		if notFound(err) || errors.Is(err, syscall.ESRCH) {
			return Process{}, procGone
		}
		return Process{}, procUnreadable
	}
	p := Process{PID: pid, Comm: strings.TrimRight(string(comm), "\n")}
	raw, err := readLimited(filepath.Join(dir, "cmdline"), maxCmdlineRead)
	switch {
	case err != nil && (notFound(err) || errors.Is(err, syscall.ESRCH)):
		return Process{}, procGone
	case err != nil:
		return Process{}, procUnreadable
	}
	if len(raw) > 0 {
		words := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
		// A server that renames itself ("nginx: master process /usr/sbin/nginx") has one word with spaces in it.
		first, _, _ := strings.Cut(string(words[0]), " ")
		p.Arg0 = filepath.Base(strings.TrimSuffix(first, ":"))
		cmd := strings.ToValidUTF8(string(bytes.Join(words, []byte{' '})), "?")
		if len(cmd) > MaxCmdlineBytes || int64(len(raw)) >= maxCmdlineRead {
			p.Cut = true
			cmd = strings.ToValidUTF8(cmd[:min(len(cmd), MaxCmdlineBytes)], "")
		}
		p.Cmdline = cmd
	}
	status, err := readLimited(filepath.Join(dir, "status"), 8192)
	if err != nil {
		if notFound(err) || errors.Is(err, syscall.ESRCH) {
			return Process{}, procGone
		}
		return Process{}, procUnreadable
	}
	p.UID = uidOfStatus(status)
	p.User = names.Users[p.UID]
	return p, procOK
}

// readLimited reads up to limit bytes of a file and returns the error, unlike readBounded.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}

// uidOfStatus reads the real uid from the "Uid:" line of /proc/<pid>/status.
func uidOfStatus(raw []byte) uint32 {
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				if n, err := strconv.ParseUint(f[0], 10, 32); err == nil {
					return uint32(n)
				}
			}
		}
	}
	return 0
}

// ---- port --------------------------------------------------------------------------------------------------------------

func (v view) port(req ObserveRequest) ObserveResponse {
	if req.Port < 1 || req.Port > requestPortMax || (req.Protocol != "tcp" && req.Protocol != "udp") {
		return ObserveResponse{Error: "bad port question"}
	}
	var socks []sock
	for _, file := range []string{req.Protocol, req.Protocol + "6"} {
		raw, err := os.ReadFile(filepath.Join(v.proc, "net", file))
		if err != nil {
			if notFound(err) {
				continue // no ipv6 in this device
			}
			return ObserveResponse{Error: "cannot read the socket table: " + errnoName(err)}
		}
		found, bad := parseNetTable(string(raw), req.Protocol, req.Port)
		if bad > 0 {
			return ObserveResponse{Error: "the socket table has lines that cannot be read"}
		}
		socks = append(socks, found...)
	}
	facts := &PortFacts{Listeners: []Listener{}}
	if len(socks) > MaxListeners {
		socks, facts.Truncated = socks[:MaxListeners], true
	}
	owners, incomplete := v.socketOwners(socks)
	facts.OwnersIncomplete = incomplete
	for _, s := range socks {
		l := Listener{Protocol: req.Protocol, Addr: s.addr}
		for _, o := range owners[s.inode] {
			l.Procs, l.Arg0s = append(l.Procs, o.Comm), append(l.Arg0s, o.Arg0)
		}
		facts.Listeners = append(facts.Listeners, l)
	}
	return ObserveResponse{Port: facts}
}

type sock struct {
	addr  string
	inode uint64
}

// parseNetTable reads /proc/net/tcp, tcp6, udp or udp6 and keeps the sockets that listen on port: tcp state LISTEN (0A), udp bound and
// not connected (07).
func parseNetTable(table, proto string, port int) (out []sock, bad int) {
	want := "0A"
	if proto == "udp" {
		want = "07"
	}
	for i, line := range strings.Split(table, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 10 {
			bad++ // a line that cannot be read could be the socket that is asked about
			continue
		}
		addr, p, ok := parseHexAddr(f[1])
		if !ok {
			bad++
			continue
		}
		if f[3] != want || p != port {
			continue
		}
		inode, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			bad++
			continue
		}
		out = append(out, sock{addr: addr, inode: inode})
	}
	return out, bad
}

// parseHexAddr reads "0100007F:1F90" (ipv4) and the 32-hex-digit ipv6 form: every 32-bit word is written in host byte order, which
// is little-endian on every platform the devices run on.
func parseHexAddr(s string) (string, int, bool) {
	host, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil || (len(host) != 8 && len(host) != 32) {
		return "", 0, false
	}
	ip := make(net.IP, len(host)/2)
	for w := 0; w < len(host)/8; w++ {
		word, err := strconv.ParseUint(host[w*8:w*8+8], 16, 32)
		if err != nil {
			return "", 0, false
		}
		ip[w*4], ip[w*4+1], ip[w*4+2], ip[w*4+3] = byte(word), byte(word>>8), byte(word>>16), byte(word>>24)
	}
	return ip.String(), int(port), true
}

// socketOwners finds the processes that hold the given sockets (by the "socket:[inode]" links of their descriptors).
func (v view) socketOwners(socks []sock) (map[uint64][]Process, bool) {
	if len(socks) == 0 {
		return nil, false
	}
	incomplete := false
	want := map[string]uint64{}
	for _, s := range socks {
		want["socket:["+strconv.FormatUint(s.inode, 10)+"]"] = s.inode
	}
	out := map[uint64][]Process{}
	ents, err := os.ReadDir(v.proc)
	if err != nil {
		return out, true
	}
	names := v.idNames()
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(v.proc, e.Name(), "fd"))
		if err != nil {
			if !notFound(err) && !errors.Is(err, syscall.ESRCH) {
				incomplete = true // a process we may not look into could hold the socket
			}
			continue
		}
		var proc *Process
		held := map[uint64]bool{}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(v.proc, e.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			if inode, ok := want[target]; ok && !held[inode] {
				if proc == nil {
					if p, st := v.process(pid, names); st != procGone && st != procUnreadable {
						proc = &p
					} else {
						break
					}
				}
				held[inode] = true
				out[inode] = append(out[inode], *proc)
			}
		}
	}
	return out, incomplete
}

// ---- kernel_param ------------------------------------------------------------------------------------------------------

var sysctlName = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_-]+)*$`)

func (v view) kernelParam(req ObserveRequest) ObserveResponse {
	if len(req.Name) > 256 || !sysctlName.MatchString(req.Name) {
		return ObserveResponse{Error: "bad sysctl name"}
	}
	p := filepath.Join(v.proc, "sys", strings.ReplaceAll(req.Name, ".", "/"))
	st, err := os.Stat(p)
	switch {
	case notFound(err):
		return ObserveResponse{KernelParam: &KernelParamFacts{}}
	case err != nil:
		return ObserveResponse{Error: "cannot look at the sysctl: " + errnoName(err)}
	case !st.Mode().IsRegular():
		return ObserveResponse{Error: "not a sysctl value"}
	}
	f, err := os.Open(p)
	if err != nil {
		return ObserveResponse{Error: "cannot read the sysctl: " + errnoName(err)}
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, MaxParamBytes+1))
	if err != nil {
		return ObserveResponse{Error: "cannot read the sysctl: " + errnoName(err)}
	}
	if len(raw) > MaxParamBytes {
		return ObserveResponse{Error: "the sysctl value is too long"}
	}
	return ObserveResponse{KernelParam: &KernelParamFacts{Exists: true, Value: strings.TrimSpace(string(raw))}}
}

// ---- request -----------------------------------------------------------------------------------------------------------

func makeRequest(req ObserveRequest) ObserveResponse {
	if req.Port < 1 || req.Port > requestPortMax || req.Host == "" || len(req.Host) > 253 {
		return ObserveResponse{Error: "bad request question"}
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > MaxRequestMs*time.Millisecond {
		timeout = DefaultRequestMs * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	switch req.Protocol {
	case "tcp":
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return ObserveResponse{Request: &RequestFacts{Failure: dialFailure(err)}}
		}
		_ = conn.Close()
		return ObserveResponse{Request: &RequestFacts{Connected: true}}
	case "http", "https":
		return httpRequest(ctx, req, addr)
	}
	return ObserveResponse{Error: "bad request protocol"}
}

func httpRequest(ctx context.Context, req ObserveRequest, addr string) ObserveResponse {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if (method != http.MethodGet && method != http.MethodHead) || len(req.URLPath) > maxRequestPathLen || (req.URLPath != "" && req.URLPath[0] != '/') {
		return ObserveResponse{Error: "bad request question"}
	}
	path := req.URLPath
	if path == "" {
		path = "/"
	}
	transport := &http.Transport{
		Proxy:             nil, // the proxy variables of the image are not ours to follow
		DisableKeepAlives: true,
		// The certificate of a lab service is whatever the author made; the probe is asking the device about itself.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // a service of the lab, asked from inside the lab
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		// A redirect is the answer; where it leads is not followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	hr, err := http.NewRequestWithContext(ctx, method, req.Protocol+"://"+addr+path, nil)
	if err != nil {
		return ObserveResponse{Error: "bad request question"}
	}
	hr.Header.Set("User-Agent", "cybericebox-check")
	resp, err := client.Do(hr)
	if err != nil {
		return ObserveResponse{Request: &RequestFacts{Failure: dialFailure(err)}}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxRequestBody+1))
	f := &RequestFacts{Connected: true, Status: resp.StatusCode}
	if len(body) > MaxRequestBody {
		body, f.Truncated = body[:MaxRequestBody], true
	}
	f.Body = body
	return ObserveResponse{Request: f}
}

// dialFailure names why a connection did not open, in a few fixed words.
func dialFailure(err error) string {
	var dns *net.DNSError
	var tlsErr *tls.CertificateVerificationError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "unreachable"
	case errors.As(err, &dns):
		return "resolve"
	case errors.As(err, &tlsErr), strings.Contains(err.Error(), "tls:"):
		return "tls"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "error"
}

// ---- permission --------------------------------------------------------------------------------------------------------

// permission starts the probe again as the user (the child sets the groups, the group and the user before it runs a line of code) and
// asks the kernel with faccessat(AT_EACCESS). Nothing is opened, nothing is written: it is the same question access(2) asks.
func (v view) permission(req ObserveRequest) ObserveResponse {
	if req.Path == "" || req.Path[0] != '/' || len(req.Path) > 4096 || strings.ContainsRune(req.Path, 0) {
		return ObserveResponse{Error: "bad path"}
	}
	cred, found := v.credentialOf(req.User)
	if !found {
		return ObserveResponse{Permission: &PermissionFacts{}}
	}
	exe, err := os.Executable()
	if err != nil {
		return ObserveResponse{Error: "cannot start the permission test"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, CommandCheckPerm, req.Path) //nolint:gosec // the probe itself
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		return ObserveResponse{Error: "the permission test failed to run"}
	}
	var facts PermissionFacts
	if json.Unmarshal(out, &facts) != nil {
		return ObserveResponse{Error: "the permission test gave no answer"}
	}
	if facts.Error != "" {
		return ObserveResponse{Error: facts.Error}
	}
	facts.UserFound = true
	return ObserveResponse{Permission: &facts}
}

// credentialOf resolves a user (a name of passwd, or a number) with its primary group and the groups that list it as a member.
func (v view) credentialOf(user string) (*syscall.Credential, bool) {
	a, err := resolveUser(v.etc, user)
	if err != nil {
		return nil, false
	}
	groups := []uint32{a.gid}
	if a.name != "" {
		for _, line := range strings.Split(string(readBounded(filepath.Join(v.etc, "group"))), "\n") {
			f := strings.Split(line, ":")
			if len(f) < 4 {
				continue
			}
			g, err := strconv.ParseUint(f[2], 10, 32)
			if err != nil || uint32(g) == a.gid {
				continue
			}
			for _, member := range strings.Split(f[3], ",") {
				if member == a.name {
					groups = append(groups, uint32(g))
					break
				}
			}
		}
	}
	return &syscall.Credential{Uid: a.uid, Gid: a.gid, Groups: groups}, true
}

// RunPermChild is the child of a permission test: it already runs as the user, asks the kernel three questions and prints the answer.
func RunPermChild(path string) int { return writeJSON(permFacts(path)) }

// permFacts asks for the user the process runs as. Only the kernel's own refusals count as "no": EACCES, EPERM, EROFS (for write) and
// ETXTBSY (for write). Any other error (an I/O error, a loop of links, a name too long) is not an answer, and is reported as an error
// so that "may not read" never passes on a failure.
func permFacts(path string) PermissionFacts {
	var f PermissionFacts
	if _, err := os.Stat(path); err != nil {
		switch {
		case notFound(err):
			return f
		case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
			f.Exists = true // it is there, or at least the user may not look: the answer for every access is no
			return f
		default:
			f.Error = "cannot look at the path: " + errnoName(err)
			return f
		}
	}
	f.Exists = true
	ask := func(mode uint32, write bool) bool {
		err := unix.Faccessat(unix.AT_FDCWD, path, mode, unix.AT_EACCESS)
		switch {
		case err == nil:
			return true
		case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM), write && (errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.ETXTBSY)):
			return false
		}
		f.Error = "the access test failed: " + errnoName(err)
		return false
	}
	f.Read, f.Write, f.Execute = ask(unix.R_OK, false), ask(unix.W_OK, true), ask(unix.X_OK, false)
	return f
}
