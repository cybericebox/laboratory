//go:build linux

package nodeagent

import (
	"context"
	"testing"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cybericebox/laboratory/internal/accessroute"
)

type podListCounter struct {
	client.Client
	returned int
}

func (c *podListCounter) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if pods, ok := list.(*corev1.PodList); ok {
		c.returned += len(pods.Items)
	}
	return nil
}

func TestAccessPortScanScopesReadsAndRetiresGoneUIDs(t *testing.T) {
	local := accessPod("local", "n1", "NET_ADMIN")
	remote := accessPod("remote", "n2", "NET_ADMIN")
	remote.UID = "remote-uid"
	c := &podListCounter{Client: fake.NewClientBuilder().WithObjects(local, remote).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).Build()}
	checks := 0
	r := &AccessPortChecker{Client: c, NodeName: "n1",
		netnsOf: func(context.Context, string) (string, error) { return "test", nil },
		ensure: func(string, string, []netlink.Route) (accessroute.Result, error) {
			checks++
			return accessroute.Result{}, nil
		},
	}
	r.Check(context.Background())
	if c.returned != 1 || checks != 1 || len(r.state) != 1 {
		t.Fatalf("local scan returned %d pods, checked %d, retained %d UIDs", c.returned, checks, len(r.state))
	}
	// Other consumers still need remote Pod placement: do not scope the shared cache.
	var all corev1.PodList
	if err := c.Client.List(context.Background(), &all); err != nil || len(all.Items) != 2 {
		t.Fatalf("remote pods no longer visible: %v, count=%d", err, len(all.Items))
	}
	if err := c.Delete(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	r.Check(context.Background())
	if checks != 1 || len(r.state) != 0 {
		t.Fatalf("gone local UID was not retired: checks=%d state=%v", checks, r.state)
	}
}
