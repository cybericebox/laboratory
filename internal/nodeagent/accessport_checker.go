//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/accessroute"
	"github.com/cybericebox/laboratory/internal/names"
)

// accessPortInterval is how often the source routing of the access ports is checked: one netlink dump per pod, cheap.
const accessPortInterval = 10 * time.Second

// AccessPortChecker keeps the source routing of the access port (see package accessroute) in place in the pods whose device may
// change it from inside: a pod with NET_ADMIN (the extended profile, or the capabilities of an in-image DHCP client) can flush
// the table, delete the rule or the port. A pod without NET_ADMIN cannot, and is left alone: cni-gate set it up once and for all.
// A lost rule or table is restored; a lost port cannot be (the device is fixed with "Reset device"): it is logged once and put
// on the pod as a Warning event, and the device status stays the operator's.
type AccessPortChecker struct {
	Client   client.Client
	NodeName string
	CRISock  string
	Recorder record.EventRecorder
	// Interval is the period of the check; zero means accessPortInterval.
	Interval time.Duration

	// Seams of the tests.
	netnsOf func(ctx context.Context, podUID string) (string, error)
	ensure  func(netnsPath, ifname string, known []netlink.Route) (accessroute.Result, error)

	mu    sync.Mutex
	state map[types.UID]*accessPortState
}

type accessPortState struct {
	netns string
	known []netlink.Route
	lost  bool // the port is gone and it was reported
}

// NeedLeaderElection: every node-agent looks after the pods of its own node.
func (c *AccessPortChecker) NeedLeaderElection() bool { return false }

// Start blocks until ctx ends.
func (c *AccessPortChecker) Start(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = accessPortInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.Check(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Check runs one round over the pods of the node.
func (c *AccessPortChecker) Check(ctx context.Context) {
	log := ctrl.Log.WithName("access-port")
	var pods corev1.PodList
	if err := c.Client.List(ctx, &pods, client.MatchingFields{"spec.nodeName": c.NodeName}); err != nil {
		log.Error(err, "list pods")
		return
	}
	seen := map[types.UID]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !needsAccessPortCheck(pod, c.NodeName) {
			continue
		}
		seen[pod.UID] = true
		c.checkPod(ctx, pod)
	}
	c.mu.Lock()
	for uid := range c.state {
		if !seen[uid] {
			delete(c.state, uid)
		}
	}
	c.mu.Unlock()
}

// needsAccessPortCheck is true for a running platform pod of this node that has the access port and may change its routes.
func needsAccessPortCheck(pod *corev1.Pod, node string) bool {
	if pod.Spec.NodeName != node || pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	if pod.Annotations[AnnotationDefaultNetwork] != names.AccessPortIface || !isPlatformPod(pod) {
		return false
	}
	return hasNetAdmin(pod)
}

func hasNetAdmin(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
			continue
		}
		for _, cp := range c.SecurityContext.Capabilities.Add {
			if cp == "NET_ADMIN" || cp == "ALL" {
				return true
			}
		}
	}
	return false
}

func (c *AccessPortChecker) checkPod(ctx context.Context, pod *corev1.Pod) {
	log := ctrl.Log.WithName("access-port").WithValues("pod", client.ObjectKeyFromObject(pod))
	c.mu.Lock()
	if c.state == nil {
		c.state = map[types.UID]*accessPortState{}
	}
	st := c.state[pod.UID]
	if st == nil {
		st = &accessPortState{}
		c.state[pod.UID] = st
	}
	c.mu.Unlock()

	if st.netns == "" {
		find := c.netnsOf
		if find == nil {
			find = func(ctx context.Context, uid string) (string, error) { return PodNetNSFromCRI(ctx, c.CRISock, uid) }
		}
		path, err := find(ctx, string(pod.UID))
		if err != nil {
			log.V(1).Info("sandbox not found", "err", err.Error())
			return
		}
		st.netns = path
	}
	ensure := c.ensure
	if ensure == nil {
		ensure = accessroute.Ensure
	}
	res, err := ensure(st.netns, names.AccessPortIface, st.known)
	switch {
	case errors.Is(err, accessroute.ErrNoPort):
		if !st.lost {
			st.lost = true
			log.Info("WARNING: the access port is gone from the pod; the publication is down until the device is reset", "iface", names.AccessPortIface)
			if c.Recorder != nil {
				c.Recorder.Event(pod, corev1.EventTypeWarning, "AccessPortLost",
					"the access port was removed from inside the device; reset the device to publish it again")
			}
		}
	case err != nil:
		// The netns may be a new one (the sandbox was recreated): look it up again next time.
		st.netns = ""
		log.Error(err, "check the source routing of the access port")
	default:
		st.lost = false
		if len(res.Routes) > 0 {
			st.known = res.Routes
		}
		if res.Changed {
			log.Info("restored the source routing of the access port", "iface", names.AccessPortIface)
		}
	}
}
