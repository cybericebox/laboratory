//go:build linux

package deviceaccess

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Containerd is the Runtime over the native containerd client: the exec is task.Exec with FIFOs in a host directory, not the CRI
// Exec (its stream server listens on the host's loopback, out of reach of a pod without host networking) and not the API server's
// pods/exec (the load would fall on the API server). Nothing opens a network port: the whole path is the unix socket and the FIFOs.
//
// The directory of the FIFOs must be the same path in the host and in the pod: the client makes the FIFOs, and the runtime's shim,
// which runs on the host, opens them by the path in the request.
type Containerd struct {
	client *containerd.Client
	// fifos holds the FIFOs of the running execs; journal holds one file per running exec, so that execs left behind by a crash of the
	// service are found and removed on the next start (containerd does not kill an exec when its client goes away).
	fifos, journal string
}

const (
	// ExecPrefix starts the id of every exec the service makes; it is how its own execs are told from any other.
	execPrefix = "cib-"
	// dirName is the directory of the service inside the FIFO directory the chart mounts. The service owns it: it empties it on start,
	// and so never removes the FIFOs of another client of the runtime.
	dirName = "cib-device-access"
)

// DialContainerd connects to the runtime's socket. namespace is the containerd namespace of the kubelet's CRI (k8s.io).
func DialContainerd(sock, namespace, fifoDir string) (*Containerd, error) {
	client, err := containerd.New(sock, containerd.WithDefaultNamespace(namespace))
	if err != nil {
		return nil, fmt.Errorf("connect to the container runtime %s: %w", sock, err)
	}
	base := filepath.Join(fifoDir, dirName)
	c := &Containerd{client: client, fifos: filepath.Join(base, "fifo"), journal: filepath.Join(base, "journal")}
	for _, d := range []string{c.fifos, c.journal} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("make %s: %w", d, err)
		}
	}
	return c, nil
}

// Close closes the connection.
func (c *Containerd) Close() error { return c.client.Close() }

// Sweep removes what a previous run of the service left: execs still running in devices (found by the journal) and stale FIFOs.
// It is called once at start, before the first stream.
func (c *Containerd) Sweep(ctx context.Context) (execs, fifos int, err error) {
	entries, err := os.ReadDir(c.journal)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		raw, rerr := os.ReadFile(filepath.Join(c.journal, e.Name()))
		_ = os.Remove(filepath.Join(c.journal, e.Name()))
		if rerr != nil || !strings.HasPrefix(e.Name(), execPrefix) {
			continue
		}
		if c.removeExec(ctx, strings.TrimSpace(string(raw)), e.Name()) {
			execs++
		}
	}
	left, err := os.ReadDir(c.fifos)
	if err != nil {
		return execs, 0, err
	}
	for _, e := range left {
		if os.RemoveAll(filepath.Join(c.fifos, e.Name())) == nil {
			fifos++
		}
	}
	return execs, fifos, nil
}

func (c *Containerd) removeExec(ctx context.Context, containerID, execID string) bool {
	cont, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		return false
	}
	task, err := cont.Task(ctx, nil)
	if err != nil {
		return false
	}
	p, err := task.LoadProcess(ctx, execID, nil)
	if err != nil {
		return false
	}
	_, err = p.Delete(ctx, containerd.WithProcessKill)
	return err == nil
}

func (c *Containerd) ListByLabels(ctx context.Context, match map[string]string) ([]ContainerInfo, error) {
	keys := make([]string, 0, len(match))
	for k := range match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// One filter string is an AND of its comma-separated parts; several filter strings would be an OR.
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, "labels."+strconv.Quote(k)+"=="+strconv.Quote(match[k]))
	}
	list, err := c.client.ContainerService().List(ctx, strings.Join(parts, ","))
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(list))
	for _, ct := range list {
		out = append(out, ContainerInfo{ID: ct.ID, Labels: ct.Labels})
	}
	return out, nil
}

func (c *Containerd) Running(ctx context.Context, id string) (bool, error) {
	cont, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	task, err := cont.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	st, err := task.Status(ctx)
	if err != nil {
		return false, err
	}
	return st.Status == containerd.Running, nil
}

func (c *Containerd) Spec(ctx context.Context, id string) (*specs.Spec, error) {
	cont, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	return cont.Spec(ctx)
}

func (c *Containerd) Start(ctx context.Context, id string, proc *specs.Process, stdio IO) (Exec, error) {
	cont, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	task, err := cont.Task(ctx, nil)
	if err != nil {
		return nil, err
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	execID := execPrefix + hex.EncodeToString(raw[:])
	journalFile := filepath.Join(c.journal, execID)
	if err := os.WriteFile(journalFile, []byte(id), 0o600); err != nil {
		return nil, fmt.Errorf("journal the exec: %w", err)
	}
	opts := []cio.Opt{cio.WithStreams(stdio.Stdin, stdio.Stdout, stdio.Stderr), cio.WithFIFODir(c.fifos)}
	if stdio.Terminal {
		opts = append(opts, cio.WithTerminal)
	}
	p, err := task.Exec(ctx, execID, proc, cio.NewCreator(opts...))
	if err != nil {
		_ = os.Remove(journalFile)
		return nil, err
	}
	e := &execProc{p: p, journalFile: journalFile, done: make(chan ExitStatus, 1)}
	// Wait is registered before Start so the exit cannot be missed.
	exit, err := p.Wait(ctx)
	if err != nil {
		_ = e.Close(ctx)
		return nil, err
	}
	if err := p.Start(ctx); err != nil {
		_ = e.Close(ctx)
		return nil, err
	}
	go func() {
		es := <-exit
		code, _, werr := es.Result()
		// The exit is reported once the output has been copied out, so the last bytes of a short command are not lost.
		ioDone := make(chan struct{})
		go func() { p.IO().Wait(); close(ioDone) }()
		select {
		case <-ioDone:
		case <-time.After(5 * time.Second):
		}
		e.done <- ExitStatus{Code: code, Err: werr}
		close(e.done)
	}()
	return e, nil
}

type execProc struct {
	p           containerd.Process
	journalFile string
	done        chan ExitStatus

	once sync.Once
	err  error
}

func (e *execProc) Wait() <-chan ExitStatus { return e.done }

func (e *execProc) Resize(ctx context.Context, cols, rows uint16) error {
	return e.p.Resize(ctx, uint32(cols), uint32(rows))
}

// Close kills the process, deletes it from the runtime, closes its IO (which removes the FIFOs) and forgets the journal entry. The
// journal entry is kept when the runtime could not delete the exec, so the next start tries again.
func (e *execProc) Close(ctx context.Context) error {
	e.once.Do(func() {
		var errs []error
		if err := e.p.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) && !errdefs.IsFailedPrecondition(err) {
			errs = append(errs, fmt.Errorf("kill: %w", err))
		}
		if _, err := e.p.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete: %w", err))
		}
		// Delete has already closed the IO of the process, which removes the FIFOs; a second close is only a safety net for the case where
		// the delete failed, and its error says nothing the delete did not.
		if io := e.p.IO(); io != nil {
			io.Cancel()
			_ = io.Close()
		}
		e.err = errors.Join(errs...)
		if e.err == nil {
			_ = os.Remove(e.journalFile)
		}
	})
	return e.err
}
