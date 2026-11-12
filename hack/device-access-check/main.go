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
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

var (
	addr    = flag.String("addr", os.Getenv("LISTEN"), "address of the service (the LISTEN of the pod: <pod ip>:<port>)")
	secret  = flag.String("secret-file", "/etc/device-access/auth/current", "the secret shared with the proxy")
	ns      = flag.String("namespace", "", "namespace of the lab group")
	labName = flag.String("lab", "", "lab name")
	device  = flag.String("device", "", "device name (its lab must ask for a terminal)")
	user    = flag.String("user", "", "terminal user, empty = the user of the image")
	command = flag.String("command", "", "terminal command, empty = /bin/sh")
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
