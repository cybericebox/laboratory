package deviceaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/probe"
)

// The checker socket (docs/specs/2026-11-12-device-access-and-checks.md, section 4). The lab checker of the node does not hold the container
// runtime socket: it asks this service, over a unix socket in a node directory the two pods share, to run the in-device probe with a check
// subcommand in a lab device. What this endpoint can do is fixed here and nowhere else:
//
//   - the program is the probe binary of the platform (names.ProbeBinary), the subcommand one of three check subcommands; no other binary,
//     no other subcommand, no argument vector, no environment from the caller;
//   - the container is found by the white list of Resolve (a lab device, never the VPN, the gateway or a system pod), by namespace, lab and
//     device, never by a container id;
//   - the process always runs as root in the device (the probe drops to the user of a custom check itself); a request has no user field, and
//     a field the service does not know is an error;
//   - one check per lab at a time and a few per node, and the time limit has a ceiling.
//
// The protection of the socket is its file permissions (a group the checker pod runs as) and the peer credentials of the connection, not
// the HMAC key of the proxy.

// CheckerLimits are the values of the checker socket.
type CheckerLimits struct {
	// MaxSeconds is the ceiling of the time one probe run may take, whatever the request says.
	MaxSeconds int
	// MaxConcurrent is the most probe runs at once on this node.
	MaxConcurrent int
}

const (
	maxProbeArg    = 1 << 20
	maxProbeOutput = 4 << 20
)

// probeGrace is added to the time limit for the exec itself (the probe kills the command at its own limit and answers).
var probeGrace = 5 * time.Second

var checkSubcommands = map[string]bool{probe.CommandCheckExec: true, probe.CommandCheckFile: true, probe.CommandCheckObserve: true}

type checkerAPI struct {
	s   *Service
	lim CheckerLimits

	mu      sync.Mutex
	perLab  map[string]bool
	running int
}

// CheckerHandler serves the checker socket. Mount it on a unix listener whose peer is checked (see ListenChecker).
func (s *Service) CheckerHandler(lim CheckerLimits) http.Handler {
	if lim.MaxSeconds <= 0 {
		lim.MaxSeconds = 120
	}
	if lim.MaxConcurrent <= 0 {
		lim.MaxConcurrent = 8
	}
	a := &checkerAPI{s: s, lim: lim, perLab: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.CheckerPath, a.probe)
	return mux
}

func answer(w http.ResponseWriter, status int, r wire.ProbeResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}

func (a *checkerAPI) acquire(lab string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.perLab[lab] || a.running >= a.lim.MaxConcurrent {
		return false
	}
	a.perLab[lab] = true
	a.running++
	return true
}

func (a *checkerAPI) release(lab string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.perLab, lab)
	a.running--
}

// validateProbe checks a request against what the endpoint may do.
func validateProbe(r wire.ProbeRequest) (string, error) {
	switch {
	case !checkSubcommands[r.Command]:
		return wire.CodeForbidden, errors.New("this subcommand of the probe is not allowed")
	case len(r.Arg) == 0 || len(r.Arg) > maxProbeArg:
		return wire.CodeBadRequest, errors.New("the argument is empty or too large")
	case r.Namespace == "" || r.Lab == "" || r.Device == "" || len(r.Namespace) > maxName || len(r.Lab) > maxName || len(r.Device) > maxName:
		return wire.CodeBadRequest, errors.New("namespace, lab and device are required")
	case r.TimeoutMs <= 0:
		return wire.CodeBadRequest, errors.New("the time limit is required")
	}
	return "", nil
}

func (a *checkerAPI) probe(w http.ResponseWriter, r *http.Request) {
	s := a.s
	var req wire.ProbeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxProbeArg+4096))
	// An unknown field is refused: there is no field for a user, a program or a container, and one cannot be added by a caller.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		answer(w, http.StatusBadRequest, wire.ProbeResponse{Error: wire.CodeBadRequest})
		return
	}
	if code, err := validateProbe(req); err != nil {
		s.log.Info("checker request refused", "code", code, "reason", err.Error())
		answer(w, http.StatusBadRequest, wire.ProbeResponse{Error: code})
		return
	}
	lab := req.Namespace + "/" + req.Lab
	if !a.acquire(lab) {
		answer(w, http.StatusTooManyRequests, wire.ProbeResponse{Error: wire.CodeBusy})
		return
	}
	defer a.release(lab)

	dev, err := ResolveLabDevice(r.Context(), s.rt, Target{Namespace: req.Namespace, Lab: req.Lab, Device: req.Device})
	if err != nil {
		code := wire.CodeUnavailable
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrAmbiguous) || errors.Is(err, ErrDisabled) {
			code = wire.CodeNotFound
		}
		answer(w, http.StatusNotFound, wire.ProbeResponse{Error: code})
		return
	}
	spec, err := s.rt.Spec(r.Context(), dev.ContainerID)
	if err != nil || spec.Process == nil {
		answer(w, http.StatusBadGateway, wire.ProbeResponse{Error: wire.CodeUnavailable})
		return
	}
	// Always root in the device: the probe needs it to read what a participant made unreadable, and it drops to the user of a custom check
	// itself. The capability sets are the container's (ProcessFor never builds them).
	proc, err := ProcessFor(spec.Process, ExecArgs{Args: []string{names.ProbeBinary, req.Command, req.Arg}, User: &ExecUser{UID: 0, GID: 0}})
	if err != nil {
		answer(w, http.StatusBadGateway, wire.ProbeResponse{Error: wire.CodeUnavailable})
		return
	}
	limit := min(time.Duration(req.TimeoutMs)*time.Millisecond, time.Duration(a.lim.MaxSeconds)*time.Second) + probeGrace
	ctx, cancel := context.WithTimeout(r.Context(), limit)
	defer cancel()
	var out limitedBuffer
	out.max = maxProbeOutput
	ex, err := s.rt.Start(ctx, dev.ContainerID, proc, IO{Stdout: &out, Stderr: &limitedBuffer{max: 0}})
	if err != nil {
		answer(w, http.StatusBadGateway, wire.ProbeResponse{Error: wire.CodeUnavailable})
		return
	}
	defer func() {
		cctx, c := context.WithTimeout(context.Background(), s.lim.Cleanup)
		defer c()
		if err := ex.Close(cctx); err != nil {
			s.log.Error(err, "clean up the probe exec", "lab", req.Lab, "device", req.Device)
		}
	}()
	select {
	case st := <-ex.Wait():
		if st.Err != nil {
			answer(w, http.StatusBadGateway, wire.ProbeResponse{Error: wire.CodeUnavailable})
			return
		}
		answer(w, http.StatusOK, wire.ProbeResponse{Exit: int(st.Code), Stdout: bytes.Clone(out.Bytes())})
	case <-ctx.Done():
		answer(w, http.StatusGatewayTimeout, wire.ProbeResponse{Error: wire.CodeTimeout})
	}
}

// PeerCheck decides whether the process on the other end of an accepted unix connection may use the checker socket.
type PeerCheck func(c net.Conn) error

// checkedListener closes a connection whose peer the check refuses.
type checkedListener struct {
	net.Listener
	check PeerCheck
}

func (l checkedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.check == nil || l.check(c) == nil {
			return c, nil
		}
		_ = c.Close()
	}
}
