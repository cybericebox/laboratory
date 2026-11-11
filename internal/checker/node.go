//go:build unix

package checker

import (
	"context"
	"path/filepath"
)

// NodeBackend gives access to the devices of this node.
//
//   - A container device: files are read from the node, from the root directory the runtime keeps for the container
//     (<state>/io.containerd.runtime.v2.task/<namespace>/<container id>/rootfs, mounted read-only into the checker with mount
//     propagation), without running anything in the container and with the participant unable to interfere; commands run
//     through the runtime's exec with the in-device tool.
//   - A micro-VM device (Kata): the root of the container is inside the guest, so files and commands both go through the
//     in-device tool and the runtime's exec (the runtime executes it in the guest).
type NodeBackend struct {
	CRI CRI
	// StateDir is the containerd state directory on the node (for k0s /run/k0s/containerd); Namespace is the containerd namespace
	// of the kubelet's CRI (k8s.io).
	StateDir  string
	Namespace string
}

// RootDir is where the node sees the root file system of a container.
func (b *NodeBackend) RootDir(containerID string) string {
	return filepath.Join(b.StateDir, "io.containerd.runtime.v2.task", b.Namespace, containerID, "rootfs")
}

// Open implements Backend.
func (b *NodeBackend) Open(ctx context.Context, pod PodInfo) (Observer, func(), error) {
	c, err := b.CRI.FindContainer(ctx, pod.UID, pod.Device)
	if err != nil {
		return nil, nil, err
	}
	exec := &execObserver{cri: b.CRI, containerID: c.ID}
	if pod.MicroVM {
		return exec, func() {}, nil
	}
	fs, err := OpenRootFS(b.RootDir(c.ID))
	if err != nil {
		return nil, nil, &ObserveError{Reason: "the node cannot read the files of the container", Err: err}
	}
	return &nodeObserver{fs: fs, ids: c.IDs, exec: exec}, fs.Close, nil
}

// nodeObserver reads files from the node and runs commands through the exec observer.
type nodeObserver struct {
	fs   *RootFS
	ids  IDMap
	exec *execObserver
}

func (o *nodeObserver) ReadFile(ctx context.Context, path string, wantContent bool) (FileFacts, error) {
	if err := ctx.Err(); err != nil {
		return FileFacts{}, err
	}
	f, err := o.fs.Observe(path, wantContent, o.ids)
	if err != nil {
		return FileFacts{}, err
	}
	if f.Exists {
		o.fs.Names(&f)
	}
	return f, nil
}

func (o *nodeObserver) Exec(ctx context.Context, s ExecSpec) (ExecResult, error) {
	return o.exec.Exec(ctx, s)
}
