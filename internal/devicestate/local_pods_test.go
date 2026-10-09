package devicestate

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type podReadCounter struct {
	client.Client
	returned int
}

func (c *podReadCounter) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if pods, ok := list.(*corev1.PodList); ok {
		c.returned += len(pods.Items)
	}
	return nil
}

func TestStatePodScanDoesNotCopyOtherNodesPods(t *testing.T) {
	k, raw := kubeRig(t, stateDevice(), statePod("local", "node-a", corev1.PodRunning), statePod("remote", "node-b", corev1.PodRunning))
	c := &podReadCounter{Client: raw}
	k.Client = c
	pods, err := k.Pods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.returned != 1 || len(pods) != 1 || pods[0].Pod != "local" {
		t.Fatalf("copied %d pods, selected %+v", c.returned, pods)
	}
	var all corev1.PodList
	if err := raw.List(context.Background(), &all); err != nil || len(all.Items) != 2 {
		t.Fatalf("shared cache lost remote pods: %v, count=%d", err, len(all.Items))
	}
}
