package deviceaccess

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/probe"
)

func probeBody(mut func(map[string]any)) []byte {
	m := map[string]any{"namespace": testNS, "lab": testLab, "device": testDev, "command": probe.CommandCheckObserve, "arg": `{"kind":"port"}`, "timeoutMs": 5000}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func callProbe(h http.Handler, body []byte) (int, wire.ProbeResponse) {
	req := httptest.NewRequest("POST", wire.CheckerPath, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r wire.ProbeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r
}

// printing is a probe that answers with one JSON document.
func printing(out string, code uint32) func(*specs.Process, IO, *fakeExec) {
	return func(_ *specs.Process, stdio IO, ex *fakeExec) {
		_, _ = stdio.Stdout.Write([]byte(out))
		ex.exit(code)
	}
}

func checkerHandler(rt Runtime) http.Handler {
	return testService(rt).CheckerHandler(CheckerLimits{MaxSeconds: 10, MaxConcurrent: 2})
}

func TestCheckerRunsOnlyTheProbeAsRootInADevice(t *testing.T) {
	rt := labRuntime()
	rt.behaviour = printing(`{"ok":true}`, 0)
	code, r := callProbe(checkerHandler(rt), probeBody(nil))
	if code != 200 || r.Error != "" || string(r.Stdout) != `{"ok":true}` || r.Exit != 0 {
		t.Fatalf("%d %+v", code, r)
	}
	ex := rt.started()
	if len(ex) != 1 || ex[0].closes.Load() != 1 {
		t.Fatalf("execs %d (closed %d)", len(ex), ex[0].closes.Load())
	}
	p := ex[0].proc
	if len(p.Args) != 3 || p.Args[0] != names.ProbeBinary || p.Args[1] != probe.CommandCheckObserve || p.Args[2] != `{"kind":"port"}` {
		t.Fatalf("args %v", p.Args)
	}
	if p.User.UID != 0 || p.User.GID != 0 || p.Terminal {
		t.Fatalf("user %+v terminal %v", p.User, p.Terminal)
	}
	// the capability sets are the container's: root of the container, nothing wider
	bound := map[string]bool{}
	for _, c := range deviceSpec().Process.Capabilities.Bounding {
		bound[c] = true
	}
	for _, c := range append(append([]string{}, p.Capabilities.Effective...), p.Capabilities.Permitted...) {
		if !bound[c] {
			t.Fatalf("%s is wider than the container", c)
		}
	}
	if !p.NoNewPrivileges {
		t.Fatal("no_new_privs was lost")
	}
}

// Even when the container runs as an ordinary user, the probe runs as root in it, with the container's own sets.
func TestCheckerProbeIsRootEvenForAnOrdinaryUserImage(t *testing.T) {
	rt := labRuntime()
	rt.containers[1].spec.Process.User.UID = 1000
	rt.behaviour = printing(`{}`, 0)
	if code, _ := callProbe(checkerHandler(rt), probeBody(nil)); code != 200 {
		t.Fatal(code)
	}
	p := rt.started()[0].proc
	if p.User.UID != 0 {
		t.Fatal("not root")
	}
	// a root with the powers of the device (the bounding set), so the probe can drop to the check's user and read a file of another user
	bound := deviceSpec().Process.Capabilities.Bounding
	if len(p.Capabilities.Effective) != len(bound) || len(p.Capabilities.Permitted) != len(bound) || len(p.Capabilities.Ambient) != 0 || len(p.Capabilities.Inheritable) != 0 {
		t.Fatalf("effective %v permitted %v ambient %v inheritable %v, want the bounding set %v", p.Capabilities.Effective, p.Capabilities.Permitted, p.Capabilities.Ambient, p.Capabilities.Inheritable, bound)
	}
}

func TestCheckerRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		body   func() []byte
		rt     func(*fakeRuntime)
		status int
		code   string
	}{
		"another binary (a program field)": {body: func() []byte { return probeBody(func(m map[string]any) { m["program"] = "/bin/sh" }) }, status: 400, code: wire.CodeBadRequest},
		"a command line": {body: func() []byte {
			return probeBody(func(m map[string]any) { m["args"] = []string{"/bin/sh", "-c", "id"} })
		}, status: 400, code: wire.CodeBadRequest},
		"a user other than root":     {body: func() []byte { return probeBody(func(m map[string]any) { m["user"] = "1000" }) }, status: 400, code: wire.CodeBadRequest},
		"an environment":             {body: func() []byte { return probeBody(func(m map[string]any) { m["env"] = []string{"A=B"} }) }, status: 400, code: wire.CodeBadRequest},
		"a container id":             {body: func() []byte { return probeBody(func(m map[string]any) { m["container"] = cont1 }) }, status: 400, code: wire.CodeBadRequest},
		"the console subcommand put": {body: func() []byte { return probeBody(func(m map[string]any) { m["command"] = probe.CommandPut }) }, status: 400, code: wire.CodeForbidden},
		"the install subcommand":     {body: func() []byte { return probeBody(func(m map[string]any) { m["command"] = "install" }) }, status: 400, code: wire.CodeForbidden},
		"the launch subcommand":      {body: func() []byte { return probeBody(func(m map[string]any) { m["command"] = "launch" }) }, status: 400, code: wire.CodeForbidden},
		"an empty subcommand":        {body: func() []byte { return probeBody(func(m map[string]any) { m["command"] = "" }) }, status: 400, code: wire.CodeForbidden},
		"an empty argument":          {body: func() []byte { return probeBody(func(m map[string]any) { m["arg"] = "" }) }, status: 400, code: wire.CodeBadRequest},
		"a huge argument": {body: func() []byte {
			return probeBody(func(m map[string]any) { m["arg"] = strings.Repeat("a", maxProbeArg+1) })
		}, status: 400, code: wire.CodeBadRequest},
		"no time limit":     {body: func() []byte { return probeBody(func(m map[string]any) { m["timeoutMs"] = 0 }) }, status: 400, code: wire.CodeBadRequest},
		"no device":         {body: func() []byte { return probeBody(func(m map[string]any) { delete(m, "device") }) }, status: 400, code: wire.CodeBadRequest},
		"not json":          {body: func() []byte { return []byte("x") }, status: 400, code: wire.CodeBadRequest},
		"a wrong namespace": {body: func() []byte { return probeBody(func(m map[string]any) { m["namespace"] = "other" }) }, status: 404, code: wire.CodeNotFound},
		"an unknown device": {body: func() []byte { return probeBody(func(m map[string]any) { m["device"] = "nope" }) }, status: 404, code: wire.CodeNotFound},
		"a system pod": {body: func() []byte {
			return probeBody(func(m map[string]any) { m["namespace"], m["lab"], m["device"] = "kube-system", "coredns", "coredns" })
		}, status: 404, code: wire.CodeNotFound},
		"the VPN pod":                       {rt: func(rt *fakeRuntime) { rt.containers[0].Labels[names.LabelComponent] = names.ComponentVPN }, status: 404, code: wire.CodeNotFound},
		"the gateway pod":                   {rt: func(rt *fakeRuntime) { rt.containers[0].Labels[names.LabelComponent] = names.ComponentGateway }, status: 404, code: wire.CodeNotFound},
		"a pod without the operator's UIDs": {rt: func(rt *fakeRuntime) { delete(rt.containers[0].Labels, names.LabelDeviceUID) }, status: 404, code: wire.CodeNotFound},
		"a container that is not running":   {rt: func(rt *fakeRuntime) { rt.containers[1].running = false }, status: 404, code: wire.CodeNotFound},
		"a helper container of the pod":     {rt: func(rt *fakeRuntime) { rt.containers[1].Labels[labelContainer] = "cib-probe" }, status: 404, code: wire.CodeNotFound},
		"labels only on the app container": {rt: func(rt *fakeRuntime) {
			rt.containers[0].Labels = map[string]string{labelKind: kindSandbox, labelPodNamespace: testNS, labelPodUID: podUID}
		}, status: 404, code: wire.CodeNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			rt := labRuntime()
			rt.behaviour = printing(`{}`, 0)
			if tc.rt != nil {
				tc.rt(rt)
			}
			body := probeBody(nil)
			if tc.body != nil {
				body = tc.body()
			}
			status, r := callProbe(checkerHandler(rt), body)
			if status != tc.status || r.Error != tc.code {
				t.Fatalf("%d %+v, want %d %s", status, r, tc.status, tc.code)
			}
			if len(rt.started()) != 0 {
				t.Fatal("something was executed for a refused request")
			}
		})
	}
}

// A console label is not needed: the checker looks at every lab device.
func TestCheckerDoesNotNeedAConsole(t *testing.T) {
	rt := labRuntime()
	delete(rt.containers[0].Labels, names.LabelTerminal)
	rt.behaviour = printing(`{}`, 0)
	if code, _ := callProbe(checkerHandler(rt), probeBody(nil)); code != 200 {
		t.Fatalf("%d", code)
	}
}

func TestCheckerOneRunPerLabAndPerNode(t *testing.T) {
	rt := labRuntime()
	release := make(chan struct{})
	rt.behaviour = func(_ *specs.Process, _ IO, ex *fakeExec) { <-release; ex.exit(0) }
	h := checkerHandler(rt)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); callProbe(h, probeBody(nil)) }()
	deadline := time.Now().Add(2 * time.Second)
	for len(rt.started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// the same lab, while its check runs: busy
	if code, r := callProbe(h, probeBody(nil)); code != 429 || r.Error != wire.CodeBusy {
		t.Fatalf("second check of a lab: %d %+v", code, r)
	}
	close(release)
	wg.Wait()
	// the slot is free again
	rt.behaviour = printing(`{}`, 0)
	if code, _ := callProbe(h, probeBody(nil)); code != 200 {
		t.Fatalf("after the first ended: %d", code)
	}
	// the node limit (2): two labs run, a third is refused
	rt2 := &fakeRuntime{behaviour: func(_ *specs.Process, _ IO, ex *fakeExec) { <-release2; ex.exit(0) }}
	for i, lab := range []string{"l1", "l2", "l3"} {
		sb := sandboxLabels(podUID + lab)
		sb[names.LabelLab] = lab
		ap := appLabels(podUID+lab, testDev)
		rt2.add(&fakeContainer{ContainerInfo: ContainerInfo{ID: "sb" + lab, Labels: sb}, running: true})
		rt2.add(&fakeContainer{ContainerInfo: ContainerInfo{ID: "c" + lab, Labels: ap}, running: true, spec: deviceSpec()})
		_ = i
	}
	h2 := checkerHandler(rt2)
	for _, lab := range []string{"l1", "l2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callProbe(h2, probeBody(func(m map[string]any) { m["lab"] = lab }))
		}()
	}
	for len(rt2.started()) < 2 && time.Now().Before(deadline.Add(2*time.Second)) {
		time.Sleep(5 * time.Millisecond)
	}
	if code, r := callProbe(h2, probeBody(func(m map[string]any) { m["lab"] = "l3" })); code != 429 || r.Error != wire.CodeBusy {
		t.Fatalf("over the node limit: %d %+v", code, r)
	}
	close(release2)
	wg.Wait()
}

var release2 = make(chan struct{})

func TestCheckerTimeoutKillsTheProbeAndHasACeiling(t *testing.T) {
	rt := labRuntime()
	rt.behaviour = func(_ *specs.Process, _ IO, ex *fakeExec) { <-ex.killed }
	svc := testService(rt)
	// a ceiling of one second (plus the grace) whatever the request asks for
	h := svc.CheckerHandler(CheckerLimits{MaxSeconds: 1, MaxConcurrent: 2})
	old := probeGraceForTest(100 * time.Millisecond)
	defer old()
	start := time.Now()
	code, r := callProbe(h, probeBody(func(m map[string]any) { m["timeoutMs"] = 3600 * 1000 }))
	if code != 504 || r.Error != wire.CodeTimeout {
		t.Fatalf("%d %+v", code, r)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the ceiling was not applied: %v", took)
	}
	if !rt.allClosed() {
		t.Fatal("the probe was not killed")
	}
}

func TestCheckerReturnsTheProbesExitCode(t *testing.T) {
	rt := labRuntime()
	rt.behaviour = printing("", 127)
	code, r := callProbe(checkerHandler(rt), probeBody(nil))
	if code != 200 || r.Exit != 127 || len(r.Stdout) != 0 {
		t.Fatalf("%d %+v", code, r)
	}
}

func TestCheckerOutputIsBounded(t *testing.T) {
	rt := labRuntime()
	rt.behaviour = printing(strings.Repeat("x", maxProbeOutput+1000), 0)
	_, r := callProbe(checkerHandler(rt), probeBody(nil))
	if len(r.Stdout) != maxProbeOutput {
		t.Fatalf("%d bytes", len(r.Stdout))
	}
}

func TestCheckerOnlyAcceptsPost(t *testing.T) {
	rt := labRuntime()
	h := checkerHandler(rt)
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		req := httptest.NewRequest(m, wire.CheckerPath, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == 200 {
			t.Errorf("%s accepted", m)
		}
	}
	req := httptest.NewRequest("POST", "/v1/other", bytes.NewReader(probeBody(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("another path: %d", rec.Code)
	}
}

// probeGraceForTest shortens the grace for a test and returns the function that restores it.
func probeGraceForTest(d time.Duration) func() {
	old := probeGrace
	probeGrace = d
	return func() { probeGrace = old }
}
