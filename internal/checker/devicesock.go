package checker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// The checker does not hold the container runtime. It asks the device access service of its node, over a unix socket in a node directory the
// two pods share, to run the in-device probe with one check subcommand in a lab device (see internal/deviceaccess/checkersock.go). The
// service decides what may run: only the probe, only its check subcommands, only in lab devices, always as root there. What this side
// sends is where to look: the device, the subcommand and its one JSON argument.

// maxAnswer bounds what is read back (the service bounds it too).
const maxAnswer = 5 << 20

// Runner runs one probe subcommand in a device and returns what it printed.
type Runner interface {
	RunProbe(ctx context.Context, pod PodInfo, command, arg string, limit time.Duration) (stdout []byte, exit int, err error)
}

// SocketRunner is the Runner over the checker socket of the service.
type SocketRunner struct {
	client *http.Client
}

// NewSocketRunner returns a Runner that talks to the unix socket at path.
func NewSocketRunner(path string) *SocketRunner {
	return &SocketRunner{client: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		DisableKeepAlives: true,
	}}}
}

// RunProbe implements Runner.
func (r *SocketRunner) RunProbe(ctx context.Context, pod PodInfo, command, arg string, limit time.Duration) ([]byte, int, error) {
	body, err := json.Marshal(wire.ProbeRequest{Namespace: pod.Lab.Namespace, Lab: pod.Lab.Name, Device: pod.Device, Command: command, Arg: arg, TimeoutMs: limit.Milliseconds()})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://device-access"+wire.CheckerPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, &ObserveError{Reason: "the device access service did not answer", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, 0, &ObserveError{Reason: "the device access service did not answer", Err: err}
	}
	var out wire.ProbeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, 0, &ObserveError{Reason: "the device access service gave no answer", Err: err}
	}
	if out.Error != "" {
		return nil, 0, &ObserveError{Reason: probeErrorReason(out.Error), Err: errors.New(out.Error)}
	}
	return out.Stdout, out.Exit, nil
}

// probeErrorReason turns a code of the service into the text of a result detail (it names no path of the node and no content).
func probeErrorReason(code string) string {
	switch code {
	case wire.CodeTimeout:
		return "the check ran over its time limit"
	case wire.CodeNotFound:
		return "the container is not running"
	case wire.CodeBusy:
		return "the node is busy with another check of this lab"
	}
	return fmt.Sprintf("the command could not be run in the device (%s)", code)
}

// SocketBackend gives access to the devices of this node through the checker socket. Every observation of every device, container or
// micro-VM, goes through the probe in the device.
type SocketBackend struct{ Runner Runner }

// Open implements Backend.
func (b *SocketBackend) Open(_ context.Context, pod PodInfo) (Observer, func(), error) {
	return &execObserver{runner: b.Runner, pod: pod}, func() {}, nil
}
