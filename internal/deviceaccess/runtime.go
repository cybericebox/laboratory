package deviceaccess

import (
	"context"
	"io"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// ContainerInfo is what the container runtime shows of a container: its id and its labels. Nothing else is trusted to find a device.
type ContainerInfo struct {
	ID     string
	Labels map[string]string
}

// IO is where the standard streams of an exec go. The runtime copies between its FIFOs and these; the service owns the other end of
// every one of them. Stderr is unused (and may be nil) when Terminal is set: a terminal has one output.
type IO struct {
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Terminal bool
}

// ExitStatus is how an exec ended.
type ExitStatus struct {
	Code uint32
	Err  error
}

// Exec is a process started in a container.
type Exec interface {
	// Wait delivers the exit status once, then closes.
	Wait() <-chan ExitStatus
	// Resize changes the size of the terminal of a TTY exec.
	Resize(ctx context.Context, cols, rows uint16) error
	// Close kills the process, deletes it from the runtime and removes its FIFOs. It is safe to call more than once and from any
	// state; the caller always calls it, whatever way the stream ended.
	Close(ctx context.Context) error
}

// Runtime is the part of the container runtime the service uses. The real one is the native containerd client (containerd_linux.go);
// tests use a fake. There is no method that takes a command or a container name from outside: the callers of this interface have
// already decided what to run.
type Runtime interface {
	// ListByLabels returns the containers whose labels contain every given pair.
	ListByLabels(ctx context.Context, match map[string]string) ([]ContainerInfo, error)
	// Running reports whether the container has a running task.
	Running(ctx context.Context, id string) (bool, error)
	// Spec returns the OCI runtime spec of the container.
	Spec(ctx context.Context, id string) (*specs.Spec, error)
	// Start runs proc in the container and starts it. The process spec is used as it is.
	Start(ctx context.Context, id string, proc *specs.Process, stdio IO) (Exec, error)
}
