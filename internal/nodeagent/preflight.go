package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cybericebox/laboratory/internal/names"
)

// What a node can do for the lifecycle of a lab is checked once, at the start of its node-agent, and published on the Node (the
// annotations names.AnnotationStatePersistence and names.AnnotationRuntimeObservation). A wrong path of the chart must not hang a node
// or break its pod networking: the node-agent still serves, the feature that cannot work is switched off in this process, and the
// reason is on the Node, in an Event and in the answer of the management agent.

const (
	// PreflightReady, PreflightDisabled and the prefix of an unavailable result are the values of the node annotations.
	PreflightReady       = "ready"
	PreflightDisabled    = "disabled"
	preflightUnavailable = "unavailable: "
)

// Unavailable is the annotation value for a check that failed: "unavailable: CODE: detail".
func Unavailable(code, detail string) string { return preflightUnavailable + code + ": " + detail }

// IsUnavailable says the value is a failed check, and gives its detail (for the operator of the cluster).
func IsUnavailable(v string) (string, bool) {
	r, ok := strings.CutPrefix(v, preflightUnavailable)
	if !ok {
		return "", false
	}
	if _, detail, found := strings.Cut(r, ": "); found {
		return detail, true
	}
	return r, true
}

// CodeOf is the reason code of an annotation value that is a failed check; a value that carries none (an older node-agent) is
// NODE_UNCONFIRMED.
func CodeOf(v string) (string, bool) {
	r, ok := strings.CutPrefix(v, preflightUnavailable)
	if !ok {
		return "", false
	}
	code, _, _ := strings.Cut(r, ": ")
	for _, c := range AllCodes {
		if code == c {
			return code, true
		}
	}
	return CodeNodeUnconfirmed, true
}

// The fixed reason codes of a failed check. They are what leaves the cluster (GetFeatures): no node names, paths or error text. The detail
// stays in the Node annotation ("unavailable: CODE: detail"), the Event and the log, for the operator of the cluster.
const (
	CodeContainerdRootMissing = "CONTAINERD_ROOT_MISSING"
	CodeSnapshotterMissing    = "SNAPSHOTTER_MISSING"
	CodeRuntimeSocket         = "RUNTIME_SOCKET_UNAVAILABLE"
	CodeSnapshotRegistry      = "SNAPSHOT_REGISTRY_UNREACHABLE"
	CodeCgroupV2Missing       = "CGROUP_V2_MISSING"
	CodePersistenceDisabled   = "PERSISTENCE_DISABLED"
	CodeObservationDisabled   = "OBSERVATION_DISABLED"
	CodeNodeUnconfirmed       = "NODE_UNCONFIRMED"
)

// AllCodes is the list of codes (the allow-list of a caller).
var AllCodes = []string{CodeContainerdRootMissing, CodeSnapshotterMissing, CodeRuntimeSocket, CodeSnapshotRegistry, CodeCgroupV2Missing,
	CodePersistenceDisabled, CodeObservationDisabled, CodeNodeUnconfirmed}

// checkError is a failed check: a code and the detail for the cluster's operator.
type checkError struct{ code, detail string }

func (e *checkError) Error() string { return e.detail }

func fail(code, format string, a ...any) error {
	return &checkError{code: code, detail: fmt.Sprintf(format, a...)}
}

// PreflightInput is what the check needs of the node-agent's configuration.
type PreflightInput struct {
	// Persistence and Observation say what is asked for.
	Persistence, Observation bool
	ContainerdRoot, CRISock  string
	CgroupRoot               string
	RegistryAddr             string
}

// PreflightResult is what the node supports.
type PreflightResult struct {
	// StatePersistence and RuntimeObservation are annotation values.
	StatePersistence, RuntimeObservation string
}

// Preflight checks, for what the config asks for, that the node has it: persistence needs the containerd root with a snapshotter, the
// CRI socket and the snapshot registry; native runtime observation needs the cgroup v2 tree and the CRI socket. Nothing here blocks for
// long (the registry is asked with a short timeout) and nothing is changed on the node.
func Preflight(ctx context.Context, cfg PreflightInput) PreflightResult {
	res := PreflightResult{StatePersistence: PreflightDisabled, RuntimeObservation: PreflightDisabled}
	if cfg.Persistence {
		res.StatePersistence = statusOf(checkPersistence(ctx, cfg))
	}
	if cfg.Observation {
		res.RuntimeObservation = statusOf(checkObservation(cfg))
	}
	return res
}

func statusOf(err error) string {
	if err != nil {
		var ce *checkError
		if errors.As(err, &ce) {
			return Unavailable(ce.code, ce.detail)
		}
		return Unavailable(CodeNodeUnconfirmed, err.Error())
	}
	return PreflightReady
}

func checkSocket(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fail(CodeRuntimeSocket, "the container runtime socket %s is not there (%v)", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fail(CodeRuntimeSocket, "%s is not a socket", path)
	}
	return nil
}

func checkPersistence(ctx context.Context, cfg PreflightInput) error {
	if cfg.ContainerdRoot == "" {
		return fail(CodeContainerdRootMissing, "CONTAINERD_ROOT is not set (devices.statePersistence.containerdRoot)")
	}
	fi, err := os.Stat(cfg.ContainerdRoot)
	if err != nil || !fi.IsDir() {
		return fail(CodeContainerdRootMissing, "the containerd root %s does not exist on this node: set devices.statePersistence.containerdRoot to the containerd root of the cluster (for example /var/lib/containerd, or /var/lib/k0s/containerd on k0s)", cfg.ContainerdRoot)
	}
	snap, _ := filepath.Glob(filepath.Join(cfg.ContainerdRoot, "io.containerd.snapshotter.v1.*"))
	if len(snap) == 0 {
		return fail(CodeSnapshotterMissing, "the containerd root %s has no snapshotter (io.containerd.snapshotter.v1.*): it is not the containerd root of this node", cfg.ContainerdRoot)
	}
	if err := checkSocket(cfg.CRISock); err != nil {
		return err
	}
	return checkRegistry(ctx, cfg.RegistryAddr)
}

// checkRegistry asks the snapshot registry for its API root: any HTTP answer that is not a server error shows it is there (401 is
// the registry asking for the credentials the node-agent has).
func checkRegistry(ctx context.Context, addr string) error {
	if addr == "" {
		return fail(CodeSnapshotRegistry, "the snapshot registry address is not set")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v2/", nil)
	if err != nil {
		return fail(CodeSnapshotRegistry, "the snapshot registry address %q: %v", addr, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail(CodeSnapshotRegistry, "the snapshot registry %s cannot be reached from this node: %v", addr, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fail(CodeSnapshotRegistry, "the snapshot registry %s answers %d", addr, resp.StatusCode)
	}
	return nil
}

func checkObservation(cfg PreflightInput) error {
	if _, err := os.Stat(filepath.Join(cfg.CgroupRoot, "cgroup.controllers")); err != nil {
		return fail(CodeCgroupV2Missing, "the host cgroup v2 tree is not at %s (CGROUP_ROOT): %v", cfg.CgroupRoot, err)
	}
	return checkSocket(cfg.CRISock)
}

// Annotations are the Node annotations of the result.
func (r PreflightResult) Annotations() map[string]string {
	return map[string]string{
		names.AnnotationStatePersistence:   r.StatePersistence,
		names.AnnotationRuntimeObservation: r.RuntimeObservation,
	}
}

// Failures are the unavailable results, one line each, for the Event and the log.
func (r PreflightResult) Failures() []string {
	var out []string
	if why, bad := IsUnavailable(r.StatePersistence); bad {
		code, _ := CodeOf(r.StatePersistence)
		out = append(out, "state persistence: "+code+": "+why)
	}
	if why, bad := IsUnavailable(r.RuntimeObservation); bad {
		code, _ := CodeOf(r.RuntimeObservation)
		out = append(out, "runtime observation: "+code+": "+why)
	}
	return out
}
