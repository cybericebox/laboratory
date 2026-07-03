//go:build linux

package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type criSandboxInfo struct {
	RuntimeSpec struct {
		Linux struct {
			Namespaces []struct {
				Type string `json:"type"`
				Path string `json:"path"`
			} `json:"namespaces"`
		} `json:"linux"`
	} `json:"runtimeSpec"`
}

// PodNetNSFromCRI queries the container runtime via CRI to get the network
// namespace path for a pod sandbox identified by podUID.
func PodNetNSFromCRI(ctx context.Context, criSock, podUID string) (string, error) {
	conn, err := grpc.NewClient("unix://"+criSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", fmt.Errorf("dial CRI %s: %w", criSock, err)
	}
	defer conn.Close()

	rt := criapi.NewRuntimeServiceClient(conn)

	// Filter on READY: a restarted pod can still have its previous NOTREADY
	// sandbox listed, and picking that one would return a dead netns path.
	list, err := rt.ListPodSandbox(ctx, &criapi.ListPodSandboxRequest{
		Filter: &criapi.PodSandboxFilter{
			LabelSelector: map[string]string{"io.kubernetes.pod.uid": podUID},
			State:         &criapi.PodSandboxStateValue{State: criapi.PodSandboxState_SANDBOX_READY},
		},
	})
	if err != nil {
		return "", fmt.Errorf("list sandboxes for pod %s: %w", podUID, err)
	}
	if len(list.Items) == 0 {
		return "", fmt.Errorf("ready sandbox not found for pod UID %s", podUID)
	}

	status, err := rt.PodSandboxStatus(ctx, &criapi.PodSandboxStatusRequest{
		PodSandboxId: list.Items[0].Id,
		Verbose:      true,
	})
	if err != nil {
		return "", fmt.Errorf("sandbox status for pod %s: %w", podUID, err)
	}

	info, ok := status.Info["info"]
	if !ok {
		return "", fmt.Errorf("no 'info' key in sandbox verbose status for pod %s", podUID)
	}

	var si criSandboxInfo
	if err := json.Unmarshal([]byte(info), &si); err != nil {
		return "", fmt.Errorf("parse sandbox info for pod %s: %w", podUID, err)
	}

	for _, ns := range si.RuntimeSpec.Linux.Namespaces {
		if ns.Type == "network" && ns.Path != "" {
			return ns.Path, nil
		}
	}
	return "", fmt.Errorf("network namespace path not found in sandbox info for pod %s", podUID)
}
