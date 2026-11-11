package checker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// ContainerInfo is a running container of a device pod.
type ContainerInfo struct {
	ID  string
	IDs IDMap
}

// CRI is the part of the node's container runtime the checker uses. Everything goes through the runtime's socket on the node,
// never through the Kubernetes API server (an exec through the API server would put the participant-facing control plane, and
// its audit trail, in the path of every check).
type CRI interface {
	// FindContainer returns the newest running container of the pod with the given name.
	FindContainer(ctx context.Context, podUID, container string) (ContainerInfo, error)
	// ExecSync runs a command in the container and waits for it.
	ExecSync(ctx context.Context, containerID string, cmd []string, timeout time.Duration) (stdout []byte, exit int32, err error)
	Close() error
}

type criClient struct {
	conn *grpc.ClientConn
	rt   criapi.RuntimeServiceClient
}

// DialCRI connects to the runtime's socket. Replies are limited to a few MiB: the answer of the in-device tool is bounded, and
// anything larger is an error, not memory of the checker.
func DialCRI(sock string) (CRI, error) {
	conn, err := grpc.NewClient("unix://"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20)))
	if err != nil {
		return nil, fmt.Errorf("dial the container runtime %s: %w", sock, err)
	}
	return &criClient{conn: conn, rt: criapi.NewRuntimeServiceClient(conn)}, nil
}

func (c *criClient) Close() error { return c.conn.Close() }

var errNoContainer = errors.New("the container is not running")

func (c *criClient) FindContainer(ctx context.Context, podUID, container string) (ContainerInfo, error) {
	list, err := c.rt.ListContainers(ctx, &criapi.ListContainersRequest{Filter: &criapi.ContainerFilter{
		State:         &criapi.ContainerStateValue{State: criapi.ContainerState_CONTAINER_RUNNING},
		LabelSelector: map[string]string{"io.kubernetes.pod.uid": podUID, "io.kubernetes.container.name": container},
	}})
	if err != nil {
		return ContainerInfo{}, &ObserveError{Reason: "the container runtime did not answer", Err: err}
	}
	if len(list.Containers) == 0 {
		return ContainerInfo{}, &ObserveError{Reason: errNoContainer.Error(), Err: errNoContainer}
	}
	sort.Slice(list.Containers, func(i, j int) bool { return list.Containers[i].CreatedAt > list.Containers[j].CreatedAt })
	id := list.Containers[0].Id
	st, err := c.rt.ContainerStatus(ctx, &criapi.ContainerStatusRequest{ContainerId: id, Verbose: true})
	if err != nil {
		return ContainerInfo{}, &ObserveError{Reason: "the container runtime did not answer", Err: err}
	}
	ids, err := idMapOf(st.GetInfo()["info"])
	if err != nil {
		return ContainerInfo{}, &ObserveError{Reason: "the user namespace of the container is not known", Err: err}
	}
	return ContainerInfo{ID: id, IDs: ids}, nil
}

// idMapOf reads linux.uidMappings and linux.gidMappings from the verbose info of a container (the OCI runtime spec).
func idMapOf(info string) (IDMap, error) {
	if info == "" {
		return IDMap{}, errors.New("the runtime gave no container info")
	}
	var doc struct {
		RuntimeSpec struct {
			Linux struct {
				UIDMappings []ociMapping `json:"uidMappings"`
				GIDMappings []ociMapping `json:"gidMappings"`
			} `json:"linux"`
		} `json:"runtimeSpec"`
	}
	if err := json.Unmarshal([]byte(info), &doc); err != nil {
		return IDMap{}, err
	}
	conv := func(in []ociMapping) []IDRange {
		out := make([]IDRange, 0, len(in))
		for _, m := range in {
			out = append(out, IDRange{ContainerID: m.ContainerID, HostID: m.HostID, Size: m.Size})
		}
		return out
	}
	return IDMap{UIDs: conv(doc.RuntimeSpec.Linux.UIDMappings), GIDs: conv(doc.RuntimeSpec.Linux.GIDMappings)}, nil
}

type ociMapping struct {
	ContainerID uint32 `json:"containerID"`
	HostID      uint32 `json:"hostID"`
	Size        uint32 `json:"size"`
}

func (c *criClient) ExecSync(ctx context.Context, id string, cmd []string, timeout time.Duration) ([]byte, int32, error) {
	resp, err := c.rt.ExecSync(ctx, &criapi.ExecSyncRequest{ContainerId: id, Cmd: cmd, Timeout: int64(timeout.Round(time.Second) / time.Second)})
	if err != nil {
		return nil, 0, err
	}
	return resp.Stdout, resp.ExitCode, nil
}
