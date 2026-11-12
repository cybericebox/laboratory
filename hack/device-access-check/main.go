//go:build ignore

// device-access-check drives a running device access service against one real lab device and reports what it saw. It is run by
// hack/device-access-check.sh INSIDE the service pod on the node of the device (the listener is bound to the pod IP and the network policy
// admits only the L7 proxy, so a port-forward would not reach it), once for a device on a runc node and once for a device on a Kata node.
// It signs its open requests with the secret the pod has mounted, so it exercises the authentication too. It was written on a machine without a Linux node and has NOT been run: treat the first run as part of the
// verification, not as a formality.
//
//	device-access-check -namespace <ns> -lab <lab> -device <device> [-user <name>] [-command /bin/sh]
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/probe"
)

var (
	addr    = flag.String("addr", os.Getenv("LISTEN"), "address of the service (the LISTEN of the pod: <pod ip>:<port>)")
	secret  = flag.String("secret-file", "/etc/device-access/auth/current", "the secret shared with the proxy")
	ns      = flag.String("namespace", "", "namespace of the lab group")
	labName = flag.String("lab", "", "lab name")
	device  = flag.String("device", "", "device name (its lab must ask for a terminal)")
	user    = flag.String("user", "", "terminal user, empty = the user of the image")
	command = flag.String("command", "", "terminal command, empty = /bin/sh")
	checker = flag.Bool("checker", false, "run the checker path instead of the console path: this must run INSIDE the checker pod (as its user), where the checker socket is")
	sockets = flag.String("checker-socket", os.Getenv("DEVICE_ACCESS_SOCKET"), "the checker socket (the DEVICE_ACCESS_SOCKET of the checker pod)")
	refuse  = flag.String("refuse", "", "comma separated namespace/lab/device triples that must be refused (the VPN pod, the gateway, a pod of kube-system, a device without a console)")
)

var failures int

func check(name string, ok bool, detail string) {
	status := "PASS"
	if !ok {
		status = "FAIL"
		failures++
	}
	fmt.Printf("%s  %s  %s\n", status, name, detail)
}

type session struct {
	c   *websocket.Conn
	buf bytes.Buffer
}

func open(ctx context.Context, req wire.OpenRequest) (*session, wire.OpenReply, error) {
	c, _, err := websocket.Dial(ctx, "ws://"+*addr+wire.StreamPath, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return nil, wire.OpenReply{}, err
	}
	c.SetReadLimit(wire.MaxFrame)
	key, err := os.ReadFile(*secret)
	if err != nil {
		return nil, wire.OpenReply{}, err
	}
	if err := wire.Sign([]byte(strings.TrimRight(string(key), "\r\n")), &req, time.Now()); err != nil {
		return nil, wire.OpenReply{}, err
	}
	raw, _ := json.Marshal(req)
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		return nil, wire.OpenReply{}, err
	}
	typ, msg, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		return nil, wire.OpenReply{}, fmt.Errorf("no reply: %v", err)
	}
	var r wire.OpenReply
	_ = json.Unmarshal(msg, &r)
	return &session{c: c}, r, nil
}

func consoleReq() wire.OpenRequest {
	r := wire.OpenRequest{Kind: wire.KindConsole, Namespace: *ns, Lab: *labName, Device: *device, User: *user, Cols: 100, Rows: 30, Client: "check", Group: "check"}
	if *command != "" {
		r.Command = strings.Fields(*command)
	}
	return r
}

// run types a line and returns what the shell printed until the marker comes back.
func (s *session) run(ctx context.Context, line string) (string, error) {
	marker := "CIBEND" + strconv.FormatInt(time.Now().UnixNano()%1e9, 10)
	if err := s.c.Write(ctx, websocket.MessageBinary, wire.Data([]byte(line+"; echo "+marker[:6]+"_"+marker[6:]+"\n"))); err != nil {
		return "", err
	}
	want := marker[:6] + "_" + marker[6:]
	s.buf.Reset()
	for {
		typ, msg, err := s.c.Read(ctx)
		if err != nil {
			return s.buf.String(), err
		}
		if typ != websocket.MessageBinary {
			continue
		}
		ft, p, perr := wire.Parse(msg)
		if perr != nil {
			return s.buf.String(), perr
		}
		if ft == wire.TypeData {
			s.buf.Write(p)
			// the echo of the typed line also contains the marker; the output line is the one that starts a line with it
			if strings.Contains(strings.ReplaceAll(s.buf.String(), "\r", ""), "\n"+want+"\n") {
				return s.buf.String(), nil
			}
		}
		if ft == wire.TypeClose {
			return s.buf.String(), fmt.Errorf("closed: %s", p)
		}
	}
}

var capRE = regexp.MustCompile(`(?m)^(CapInh|CapPrm|CapEff|CapBnd|CapAmb):\s+([0-9a-f]+)`)

func caps(out string) map[string]uint64 {
	m := map[string]uint64{}
	for _, g := range capRE.FindAllStringSubmatch(out, -1) {
		v, _ := strconv.ParseUint(g[2], 16, 64)
		m[g[1]] = v
	}
	return m
}

func main() {
	flag.Parse()
	if *ns == "" || *labName == "" || *device == "" {
		fmt.Fprintln(os.Stderr, "-namespace, -lab and -device are required")
		os.Exit(2)
	}
	if *checker {
		checkerPath()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. a console works, and the shell is in the device
	s, r, err := open(ctx, consoleReq())
	if err != nil || !r.OK {
		check("open a console", false, fmt.Sprintf("%v %+v", err, r))
		os.Exit(1)
	}
	out, err := s.run(ctx, "echo $((20+22))")
	check("a command runs and its output comes back", err == nil && strings.Contains(out, "\n42\r\n"), strings.TrimSpace(out))

	// 2. resize reaches the pty
	_ = s.c.Write(ctx, websocket.MessageBinary, wire.Resize(132, 41))
	time.Sleep(500 * time.Millisecond)
	out, err = s.run(ctx, "stty size")
	check("resize reaches the terminal (41 132)", err == nil && strings.Contains(out, "41 132"), strings.TrimSpace(out))

	// 3. the shell has no more than the device: compare with PID 1 of the container (the pid namespace is the container's, or the guest's)
	own, err1 := s.run(ctx, "grep -E '^(Cap|NoNewPrivs|Seccomp)' /proc/self/status")
	init1, err2 := s.run(ctx, "grep -E '^(Cap|NoNewPrivs|Seccomp)' /proc/1/status")
	if err1 != nil || err2 != nil {
		check("capabilities of the shell are not wider than the container's", false, fmt.Sprint(err1, err2))
	} else {
		o, i := caps(own), caps(init1)
		ok := o["CapBnd"]&^i["CapBnd"] == 0 && o["CapEff"]&^i["CapBnd"] == 0 && o["CapPrm"]&^i["CapBnd"] == 0
		check("capabilities of the shell are within the container's bounding set", ok, fmt.Sprintf("shell %x/%x bnd %x; container bnd %x", o["CapEff"], o["CapPrm"], o["CapBnd"], i["CapBnd"]))
		nnp := func(s string) string {
			m := regexp.MustCompile(`NoNewPrivs:\s+(\d)`).FindStringSubmatch(s)
			if m == nil {
				return "?"
			}
			return m[1]
		}
		check("no_new_privs is the container's", nnp(own) == nnp(init1), nnp(own)+" vs "+nnp(init1))
		fmt.Println("INFO  /proc/self/status (shell):\n" + own)
		fmt.Println("INFO  /proc/1/status (container):\n" + init1)
	}
	who, _ := s.run(ctx, "id")
	fmt.Println("INFO  id:", strings.TrimSpace(who))
	// A root console (-user root) in a device whose image runs as an ordinary user: effective and permitted must be the bounding set (a root with
	// empty sets could not drop to another user or read a file of another user). On runc the service writes that into the exec spec; on Kata the shim
	// does not pass capabilities from the spec at all and the agent gives the exec the sets of the container's init, so a failure THERE is the finding
	// to report, not a mistake of this check.
	if *user == "root" || *user == "0" {
		st, err := s.run(ctx, "grep -E '^(Uid|CapInh|CapPrm|CapEff|CapBnd|CapAmb):' /proc/self/status; grep -E '^Uid:' /proc/1/status | sed 's/^/init /'")
		if err == nil {
			fmt.Println("INFO  /proc/self/status of the root console (capsh --print equivalent):\n" + st)
			c := caps(st)
			initUID := regexp.MustCompile(`init Uid:\s+(\d+)`).FindStringSubmatch(st)
			if len(initUID) == 2 && initUID[1] != "0" {
				check("root console in a non-root image: effective == permitted == bounding", c["CapEff"] == c["CapBnd"] && c["CapPrm"] == c["CapBnd"] && c["CapBnd"] != 0 && c["CapInh"] == 0 && c["CapAmb"] == 0,
					fmt.Sprintf("eff %x prm %x bnd %x inh %x amb %x", c["CapEff"], c["CapPrm"], c["CapBnd"], c["CapInh"], c["CapAmb"]))
			} else {
				fmt.Println("INFO  the image runs as root: the sets are the container's own (unchanged)")
			}
		}
	}

	// 4. a process left running is killed when the connection drops
	_, _ = s.run(ctx, "sleep 7331 &")
	s.c.CloseNow()
	time.Sleep(3 * time.Second)
	s2, r2, err := open(ctx, consoleReq())
	if err != nil || !r2.OK {
		check("second console", false, fmt.Sprint(err, r2))
	} else {
		left, _ := s2.run(ctx, "ls /proc | grep -E '^[0-9]+$' | while read p; do tr '\\0' ' ' < /proc/$p/cmdline 2>/dev/null; echo; done | grep -c 'sleep 7331'")
		check("the exec and its children are gone after the connection dropped", strings.Contains(strings.ReplaceAll(left, "\r", ""), "\n0\n"), strings.TrimSpace(left))
		_ = s2.c.Write(ctx, websocket.MessageBinary, wire.Close(wire.ReasonClient))
		s2.c.CloseNow()
	}

	// 5. files
	payload := make([]byte, 1<<20)
	_, _ = rand.Read(payload)
	path := "/tmp/cib-check-" + strconv.FormatInt(time.Now().Unix(), 10)
	put, pr, err := open(ctx, wire.OpenRequest{Kind: wire.KindPut, Namespace: *ns, Lab: *labName, Device: *device, User: *user, Path: path, Size: int64(len(payload)), Client: "check", Group: "check"})
	if err != nil || !pr.OK {
		check("upload", false, fmt.Sprint(err, pr))
	} else {
		for b := payload; len(b) > 0; {
			n := min(len(b), wire.MaxData)
			_ = put.c.Write(ctx, websocket.MessageBinary, wire.Data(b[:n]))
			b = b[n:]
		}
		_ = put.c.Write(ctx, websocket.MessageBinary, wire.Close(wire.ReasonExit))
		typ, msg, err := put.c.Read(ctx)
		var fin wire.OpenReply
		_ = json.Unmarshal(msg, &fin)
		check("upload of 1 MiB", err == nil && typ == websocket.MessageText && fin.OK, fmt.Sprint(err, fin))
		put.c.CloseNow()
	}
	get, gr, err := open(ctx, wire.OpenRequest{Kind: wire.KindGet, Namespace: *ns, Lab: *labName, Device: *device, User: *user, Path: path, Client: "check", Group: "check"})
	if err != nil || !gr.OK {
		check("download", false, fmt.Sprint(err, gr))
	} else {
		var got bytes.Buffer
		for {
			typ, msg, err := get.c.Read(ctx)
			if err != nil || typ != websocket.MessageBinary {
				break
			}
			ft, p, _ := wire.Parse(msg)
			if ft == wire.TypeData {
				got.Write(p)
			}
			if ft == wire.TypeClose {
				break
			}
		}
		a, b := sha256.Sum256(payload), sha256.Sum256(got.Bytes())
		check("download returns the same bytes", a == b && gr.Size == int64(len(payload)), fmt.Sprintf("%d bytes, announced %d", got.Len(), gr.Size))
		get.c.CloseNow()
	}
	for _, bad := range []struct{ name, path string }{{"a directory", "/tmp"}, {"a device node", "/dev/null"}, {"a missing file", "/tmp/does-not-exist-cib"}} {
		g, rr, err := open(ctx, wire.OpenRequest{Kind: wire.KindGet, Namespace: *ns, Lab: *labName, Device: *device, User: *user, Path: bad.path, Client: "check", Group: "check"})
		check("download of "+bad.name+" is refused", err == nil && !rr.OK, fmt.Sprint(err, rr))
		if g != nil {
			g.c.CloseNow()
		}
	}

	// 6. the white list
	cases := []string{*ns + "/" + *labName + "/no-such-device", "kube-system/coredns/coredns"}
	if *refuse != "" {
		cases = append(cases, strings.Split(*refuse, ",")...)
	}
	for _, c := range cases {
		parts := strings.Split(c, "/")
		if len(parts) != 3 {
			continue
		}
		x, rr, err := open(ctx, wire.OpenRequest{Kind: wire.KindConsole, Namespace: parts[0], Lab: parts[1], Device: parts[2], Client: "check", Group: "check"})
		check("refused: "+c, err == nil && !rr.OK, fmt.Sprint(err, rr.Error))
		if x != nil {
			x.c.CloseNow()
		}
	}

	// 7. an unsigned open request is refused
	{
		c, _, err := websocket.Dial(ctx, "ws://"+*addr+wire.StreamPath, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
		if err == nil {
			raw, _ := json.Marshal(consoleReq())
			_ = c.Write(ctx, websocket.MessageText, raw)
			_, msg, rerr := c.Read(ctx)
			var rr wire.OpenReply
			_ = json.Unmarshal(msg, &rr)
			check("an unsigned open request is refused", rerr == nil && !rr.OK && rr.Error == wire.CodeUnauthorized, fmt.Sprint(rerr, rr.Error))
			c.CloseNow()
		}
	}

	if failures > 0 {
		fmt.Printf("\n%d check(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}

// ---- the checker path -------------------------------------------------------------------------------------------------------------------

// probeCall makes one request on the checker socket the way the checker does.
func probeCall(body map[string]any) (int, wire.ProbeResponse, error) {
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", *sockets)
	}, DisableKeepAlives: true}}
	raw, _ := json.Marshal(body)
	resp, err := client.Post("http://device-access"+wire.CheckerPath, "application/json", bytes.NewReader(raw))
	if err != nil {
		return 0, wire.ProbeResponse{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out wire.ProbeResponse
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out, nil
}

func target(command string, arg any, timeout time.Duration) map[string]any {
	a, _ := json.Marshal(arg)
	return map[string]any{"namespace": *ns, "lab": *labName, "device": *device, "command": command, "arg": string(a), "timeoutMs": timeout.Milliseconds()}
}

// ask runs a probe subcommand and decodes its one JSON answer into out.
func ask(command string, arg, out any) error {
	status, r, err := probeCall(target(command, arg, 20*time.Second))
	if err != nil {
		return err
	}
	if status != 200 || r.Error != "" || r.Exit != 0 {
		return fmt.Errorf("status %d error %q exit %d", status, r.Error, r.Exit)
	}
	return json.Unmarshal(r.Stdout, out)
}

func checkerPath() {
	fmt.Println("INFO  uid", os.Getuid(), "gid", os.Getgid(), "socket", *sockets, "(must be a plain user, not root)")
	check("the checker runs as a plain user", os.Getuid() != 0, fmt.Sprint(os.Getuid()))

	// one check of each kind
	var f probe.FileResponse
	err := ask(probe.CommandCheckFile, probe.FileRequest{Path: "/etc/passwd", WantContent: true}, &f)
	check("file: /etc/passwd exists and is read", err == nil && f.Exists && f.Regular && len(f.Content) > 0, fmt.Sprint(err))
	f = probe.FileResponse{}
	err = ask(probe.CommandCheckFile, probe.FileRequest{Path: "/no/such/file-cib"}, &f)
	check("file: a missing file is reported as missing", err == nil && !f.Exists, fmt.Sprint(err))
	// a file only root can read (the probe runs as root in the device, so a 0600 root file of a non-root image is readable)
	f = probe.FileResponse{}
	err = ask(probe.CommandCheckFile, probe.FileRequest{Path: "/etc/shadow"}, &f)
	fmt.Println("INFO  /etc/shadow:", f.Exists, f.Mode, err)

	var o probe.ObserveResponse
	err = ask(probe.CommandCheckObserve, probe.ObserveRequest{Kind: probe.KindKernelParam, Name: "kernel.ostype"}, &o)
	check("kernel_param: kernel.ostype", err == nil && o.KernelParam != nil && o.KernelParam.Exists, fmt.Sprint(err, o.Error))
	o = probe.ObserveResponse{}
	err = ask(probe.CommandCheckObserve, probe.ObserveRequest{Kind: probe.KindProcess}, &o)
	check("process: the process table is read", err == nil && o.Processes != nil, fmt.Sprint(err, o.Error))
	o = probe.ObserveResponse{}
	err = ask(probe.CommandCheckObserve, probe.ObserveRequest{Kind: probe.KindPort, Port: 1, Protocol: "tcp"}, &o)
	check("port: a port is looked at", err == nil && o.Port != nil, fmt.Sprint(err, o.Error))
	o = probe.ObserveResponse{}
	err = ask(probe.CommandCheckObserve, probe.ObserveRequest{Kind: probe.KindDirectory, Path: "/etc", Key: bytes.Repeat([]byte{1}, 32)}, &o)
	check("directory: /etc is listed (as keyed hashes)", err == nil && o.Directory != nil && o.Directory.Exists, fmt.Sprint(err, o.Error))
	o = probe.ObserveResponse{}
	err = ask(probe.CommandCheckObserve, probe.ObserveRequest{Kind: probe.KindPermission, User: "root", Path: "/etc/passwd"}, &o)
	check("permission: root may read /etc/passwd", err == nil && o.Permission != nil, fmt.Sprint(err, o.Error))
	o = probe.ObserveResponse{}
	err = ask(probe.CommandCheckObserve, probe.ObserveRequest{Kind: probe.KindRequest, Protocol: "tcp", Host: "127.0.0.1", Port: 1, TimeoutMs: 2000}, &o)
	check("request: a connection to a closed port is a fact, not an error", err == nil && o.Request != nil, fmt.Sprint(err, o.Error))

	// custom: as root and as nobody (the probe drops to the user itself); on a device whose image user is not root this still works
	for _, user := range []string{"root", "nobody"} {
		var e probe.ExecResponse
		err = ask(probe.CommandCheckExec, probe.ExecRequest{Argv: []string{"/bin/sh", "-c", "id -un"}, User: user, TimeoutMs: 10000}, &e)
		check("custom as "+user, err == nil && e.Started && strings.TrimSpace(string(e.Output)) == user, fmt.Sprint(err, " ", e.Error, " ", string(e.Output)))
	}
	var e probe.ExecResponse
	err = ask(probe.CommandCheckExec, probe.ExecRequest{Argv: []string{"/bin/sh", "-c", "sleep 30"}, TimeoutMs: 1000}, &e)
	check("custom over its limit is killed and reported", err == nil && e.TimedOut, fmt.Sprint(err, e.TimedOut))

	// A file of another user with mode 0600, read by the probe (root in the device): needs CAP_DAC_OVERRIDE or CAP_DAC_READ_SEARCH, which a root of
	// a non-root image only has if its sets are the bounding set. The file is made by a custom check as nobody (the probe drops to nobody itself,
	// which needs CAP_SETUID).
	var mk probe.ExecResponse
	err = ask(probe.CommandCheckExec, probe.ExecRequest{Argv: []string{"/bin/sh", "-c", "umask 077; echo cib-0600 > /tmp/cib-0600; id -un"}, User: "nobody", TimeoutMs: 10000}, &mk)
	check("custom as nobody makes a 0600 file", err == nil && mk.Started && strings.TrimSpace(string(mk.Output)) == "nobody", fmt.Sprint(err, " ", mk.Error, " ", string(mk.Output)))
	f = probe.FileResponse{}
	err = ask(probe.CommandCheckFile, probe.FileRequest{Path: "/tmp/cib-0600", WantContent: true}, &f)
	check("file: a 0600 file of another user is read (no false 'cannot read')", err == nil && f.Exists && strings.TrimSpace(string(f.Content)) == "cib-0600", fmt.Sprint(err, f.Exists, string(f.Content)))
	var capsOut probe.ExecResponse
	if err = ask(probe.CommandCheckExec, probe.ExecRequest{Argv: []string{"/bin/sh", "-c", "grep -E '^(Uid|CapInh|CapPrm|CapEff|CapBnd|CapAmb):' /proc/self/status"}, User: "root", TimeoutMs: 10000}, &capsOut); err == nil {
		fmt.Println("INFO  /proc/self/status of the probe as root (capsh --print equivalent):\n" + string(capsOut.Output))
	}
	_ = ask(probe.CommandCheckExec, probe.ExecRequest{Argv: []string{"/bin/rm", "-f", "/tmp/cib-0600"}, User: "root", TimeoutMs: 5000}, &mk)

	// what the socket must refuse
	for name, c := range map[string]struct {
		body   map[string]any
		status int
	}{
		"another subcommand (put)":   {target(probe.CommandPut, "/tmp/x", time.Second), 400},
		"the launch subcommand":      {target("launch", "x", time.Second), 400},
		"a user other than root":     {withField(target(probe.CommandCheckFile, probe.FileRequest{Path: "/etc/passwd"}, time.Second), "user", "1000"), 400},
		"a program":                  {withField(target(probe.CommandCheckFile, probe.FileRequest{Path: "/etc/passwd"}, time.Second), "program", "/bin/sh"), 400},
		"a pod of kube-system":       {map[string]any{"namespace": "kube-system", "lab": "coredns", "device": "coredns", "command": probe.CommandCheckFile, "arg": "{}", "timeoutMs": 1000}, 404},
		"a device that is not there": {map[string]any{"namespace": *ns, "lab": *labName, "device": "no-such-device", "command": probe.CommandCheckFile, "arg": "{}", "timeoutMs": 1000}, 404},
	} {
		status, r, err := probeCall(c.body)
		check("refused: "+name, err == nil && status == c.status && r.Error != "", fmt.Sprint(err, status, r.Error))
	}
	for _, x := range strings.Split(*refuse, ",") {
		parts := strings.Split(x, "/")
		if len(parts) != 3 {
			continue
		}
		status, r, err := probeCall(map[string]any{"namespace": parts[0], "lab": parts[1], "device": parts[2], "command": probe.CommandCheckFile, "arg": "{}", "timeoutMs": 1000})
		check("refused (not a lab device): "+x, err == nil && status == 404 && r.Error != "", fmt.Sprint(err, status, r.Error))
	}
	if failures > 0 {
		fmt.Printf("\n%d check(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}

func withField(m map[string]any, k string, v any) map[string]any {
	m[k] = v
	return m
}
