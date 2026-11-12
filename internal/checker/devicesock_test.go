package checker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/labchecks"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/probe"
)

// socketServer is a stand-in for the device access service on a unix socket; it records what it was sent.
func socketServer(t *testing.T, h func(r wire.ProbeRequest, raw string) (int, wire.ProbeResponse)) (string, *[]string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "chk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen = append(seen, string(raw))
		var req wire.ProbeRequest
		_ = json.Unmarshal(raw, &req)
		status, resp := h(req, string(raw))
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path, &seen
}

func jsonOf(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every observation of every device goes through the socket, and what is sent is where to look: the device, the subcommand and its one
// argument. No program, user, environment or container is part of the request.
func TestEveryObservationGoesThroughTheSocket(t *testing.T) {
	path, seen := socketServer(t, func(r wire.ProbeRequest, _ string) (int, wire.ProbeResponse) {
		switch r.Command {
		case probe.CommandCheckObserve:
			return 200, wire.ProbeResponse{Stdout: jsonOf(t, probe.ObserveResponse{KernelParam: &probe.KernelParamFacts{Exists: true, Value: "1"}})}
		case probe.CommandCheckFile:
			return 200, wire.ProbeResponse{Stdout: jsonOf(t, probe.FileResponse{Exists: true, Regular: true, Type: "file", Content: []byte("hello")})}
		}
		return 200, wire.ProbeResponse{Stdout: jsonOf(t, probe.ExecResponse{Started: true, Output: []byte("out")})}
	})
	b := &SocketBackend{Runner: NewSocketRunner(path)}
	pod := PodInfo{Lab: LabKey{Namespace: "ns", Name: "lab1"}, Device: "web", UID: "u"}
	obs, release, err := b.Open(context.Background(), pod)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for name, part := range map[string]labchecks.Part{
		"built-in": {KernelParam: &labchecks.KernelParamPart{Name: "net.ipv4.ip_forward", Equals: sp("1")}},
		"file":     {File: &labchecks.FilePart{Path: "/etc/motd", ContentContains: sp("hello")}},
		"custom":   {Custom: &labchecks.CustomPart{Command: []string{"/bin/true"}, OutputContains: sp("out")}},
	} {
		if r := RunCheck(context.Background(), check(part), obs); r.Outcome != Passed {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	if len(*seen) != 3 {
		t.Fatalf("%d requests: %v", len(*seen), *seen)
	}
	for _, raw := range *seen {
		var generic map[string]any
		_ = json.Unmarshal([]byte(raw), &generic)
		for k := range generic {
			switch k {
			case "namespace", "lab", "device", "command", "arg", "timeoutMs":
			default:
				t.Errorf("the request carries a field %q: %s", k, raw)
			}
		}
		if generic["namespace"] != "ns" || generic["lab"] != "lab1" || generic["device"] != "web" {
			t.Errorf("where to look: %s", raw)
		}
	}
	var q probe.ObserveRequest
	var first wire.ProbeRequest
	_ = json.Unmarshal([]byte((*seen)[0]), &first)
	if err := json.Unmarshal([]byte(first.Arg), &q); err != nil || q.Kind != probe.KindKernelParam || first.Arg != `{"kind":"kernel_param","name":"net.ipv4.ip_forward"}` {
		t.Fatalf("the question carries more than where to look: %s", first.Arg)
	}
}

func TestSocketErrorsBecomeSafeReasons(t *testing.T) {
	for _, c := range []struct {
		code, want string
	}{
		{wire.CodeTimeout, "over its time limit"},
		{wire.CodeNotFound, "the container is not running"},
		{wire.CodeBusy, "busy"},
		{wire.CodeUnavailable, "could not be run in the device"},
	} {
		path, _ := socketServer(t, func(wire.ProbeRequest, string) (int, wire.ProbeResponse) {
			return 500, wire.ProbeResponse{Error: c.code}
		})
		obs, _, _ := (&SocketBackend{Runner: NewSocketRunner(path)}).Open(context.Background(), PodInfo{Device: "web", Lab: LabKey{Namespace: "ns", Name: "l"}})
		r := RunCheck(context.Background(), check(labchecks.Part{KernelParam: &labchecks.KernelParamPart{Name: "a.b", Equals: sp("1")}}), obs)
		if r.Outcome != Failed || !strings.Contains(r.Detail, c.want) || strings.Contains(r.Detail, "sock") {
			t.Errorf("%s: %+v", c.code, r)
		}
	}
	// no service: a safe reason, no path of the node in it
	obs, _, _ := (&SocketBackend{Runner: NewSocketRunner("/nonexistent/dir/s.sock")}).Open(context.Background(), PodInfo{Device: "web"})
	r := RunCheck(context.Background(), check(labchecks.Part{KernelParam: &labchecks.KernelParamPart{Name: "a.b", Equals: sp("1")}}), obs)
	if r.Outcome != Failed || strings.Contains(r.Detail, "/nonexistent") || !strings.Contains(r.Detail, "did not answer") {
		t.Errorf("%+v", r)
	}
}

func TestExitOfTheProbeThatIsMissingIsNotAnAnswer(t *testing.T) {
	path, _ := socketServer(t, func(wire.ProbeRequest, string) (int, wire.ProbeResponse) { return 200, wire.ProbeResponse{Exit: 127} })
	obs, _, _ := (&SocketBackend{Runner: NewSocketRunner(path)}).Open(context.Background(), PodInfo{Device: "web"})
	r := RunCheck(context.Background(), check(labchecks.Part{KernelParam: &labchecks.KernelParamPart{Name: "a.b", Equals: sp("1")}}), obs)
	if r.Outcome != Failed || !strings.Contains(r.Detail, "in-device tool") {
		t.Errorf("%+v", r)
	}
}

func TestSocketRunnerRespectsItsContext(t *testing.T) {
	path, _ := socketServer(t, func(wire.ProbeRequest, string) (int, wire.ProbeResponse) {
		time.Sleep(2 * time.Second)
		return 200, wire.ProbeResponse{}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := NewSocketRunner(path).RunProbe(ctx, PodInfo{Device: "web"}, probe.CommandCheckFile, "{}", time.Second)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	_ = names.ProbeBinary
}
