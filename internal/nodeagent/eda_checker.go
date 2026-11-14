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

	"github.com/cybericebox/laboratory/internal/edaroute"
	"github.com/cybericebox/laboratory/internal/names"
)

// edaInterval is how often the source routing of the EDA interfaces is checked: one netlink dump per pod, cheap.
const edaInterval = 10 * time.Second

// EDAChecker keeps the source routing of the EDA interface (see package edaroute) in place in the pods whose device may
// change it from inside: a pod with NET_ADMIN (the extended profile, or the capabilities of an in-image DHCP client) can flush
// the table, delete the rule or the port. A pod without NET_ADMIN cannot, and is left alone: cni-gate set it up once and for all.
// A lost rule or table is restored; a lost port cannot be (the device is fixed with "Reset device"): it is logged once and put
// on the pod as a Warning event, and the device status stays the operator's.
type EDAChecker struct {
	Client   client.Client
	NodeName string
	CRISock  string
	Recorder record.EventRecorder
	// Interval is the period of the check; zero means edaInterval.
	Interval time.Duration

	// Seams of the tests.
	netnsOf func(ctx context.Context, podUID string) (string, error)
	ensure  func(netnsPath, ifname string, known []netlink.Route) (edaroute.Result, error)

	mu    sync.Mutex
	state map[types.UID]*edaState
}

type edaState struct {
	netns string
	known []netlink.Route
	lost  bool // the port is gone and it was reported
}

// NeedLeaderElection: every node-agent looks after the pods of its own node.
func (c *EDAChecker) NeedLeaderElection() bool { return false }

// Start blocks until ctx ends.
func (c *EDAChecker) Start(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = edaInterval
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
func (c *EDAChecker) Check(ctx context.Context) {
	log := ctrl.Log.WithName("eda")
	var pods corev1.PodList
	if err := c.Client.List(ctx, &pods, client.MatchingFields{"spec.nodeName": c.NodeName}); err != nil {
		log.Error(err, "list pods")
		return
	}
	seen := map[types.UID]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !needsEDACheck(pod, c.NodeName) {
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

// needsEDACheck is true for a running platform pod of this node that has the EDA interface and may change its routes.
func needsEDACheck(pod *corev1.Pod, node string) bool {
	if pod.Spec.NodeName != node || pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	if !names.IsEDAIface(pod.Annotations[AnnotationDefaultNetwork]) || !isPlatformPod(pod) {
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

func (c *EDAChecker) checkPod(ctx context.Context, pod *corev1.Pod) {
	log := ctrl.Log.WithName("eda").WithValues("pod", client.ObjectKeyFromObject(pod))
	c.mu.Lock()
	if c.state == nil {
		c.state = map[types.UID]*edaState{}
	}
	st := c.state[pod.UID]
	if st == nil {
		st = &edaState{}
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
		ensure = edaroute.Ensure
	}
	// A pod created before the rename still carries the old name in its annotation and as its interface.
	iface := pod.Annotations[AnnotationDefaultNetwork]
	res, err := ensure(st.netns, iface, st.known)
	switch {
	case errors.Is(err, edaroute.ErrNoPort):
		if !st.lost {
			st.lost = true
			log.Info("WARNING: the EDA interface is gone from the pod; the publication is down until the device is reset", "iface", iface)
			if c.Recorder != nil {
				c.Recorder.Event(pod, corev1.EventTypeWarning, "EDALost",
					"the EDA interface was removed from inside the device; reset the device to publish it again")
			}
		}
	case err != nil:
		// The netns may be a new one (the sandbox was recreated): look it up again next time.
		st.netns = ""
		log.Error(err, "check the source routing of the EDA interface")
	default:
		st.lost = false
		if len(res.Routes) > 0 {
			st.known = res.Routes
		}
		if res.Changed {
			log.Info("restored the source routing of the EDA interface", "iface", iface)
		}
	}
}
