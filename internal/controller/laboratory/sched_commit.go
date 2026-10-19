package laboratory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// This file is the gang commit (spec decisions 21 and 22): a gang is sent whole (sched_plan.go), and it must start as one
// commit. When some sent pod of the gang still has no node the node timeout after the gang's last dispatch, the whole gang is
// withdrawn: every pod record of it goes back to Queued with a backoff (the reconcilers then delete its workloads, the running
// ones too), so its room, quota and reservation usage are free again; it keeps its arrival. Pods that got a node and then fail
// (image pull, crash) keep the per-pod rules of observe. After a withdrawal, admission places on the nodes that exist only
// (the breaker) until a new lab node becomes schedulable or one node timeout passes.

var (
	podsWaitingForNode = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_pods_waiting_for_node",
		Help: "Lab pods that are sent and have no node yet (a node may be coming).",
	})
	capacityBreaker = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_capacity_breaker",
		Help: "1 while admission uses the current lab nodes only: a group was withdrawn because no node came within the node timeout, and no new lab node came since.",
	})
	gangWithdrawals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "laboratory_gang_withdrawals_total",
		Help: "Groups withdrawn because not every pod got a node within the node timeout.",
	})
)

func init() { metrics.Registry.MustRegister(podsWaitingForNode, capacityBreaker, gangWithdrawals) }

// repeatedWithdrawals is the attempt from which the tenant of a group gets the GroupsWithdrawnRepeatedly condition.
const repeatedWithdrawals = 3

// nodeTimeout is the node timeout of the gang commit.
func (s *Scheduler) nodeTimeout() time.Duration {
	if s.Config.NodeTimeout > 0 {
		return s.Config.NodeTimeout
	}
	return 15 * time.Minute
}

// withdrawBackoff is the wait before the n-th withdrawal of a gang may be dispatched again: 1m, 2m, 4m ..., at most the timeout.
func withdrawBackoff(n int32, timeout time.Duration) time.Duration {
	d := time.Minute
	for i := int32(1); i < n && d < timeout; i++ {
		d *= 2
	}
	return min(d, timeout)
}

// newestPod is the newest of the pods; nil for none.
func newestPod(pods []*corev1.Pod) *corev1.Pod {
	var newest *corev1.Pod
	for _, p := range pods {
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	return newest
}

// nodeless says a sent pod waits for a node: its newest pod exists, is not on a node and nothing ran in it yet. A pod that was
// never created is not waiting for a node (its workload makes the pod whatever the nodes; a quota, a refusal or a missing
// workload is a failure of the pod itself, which the startup timeout decides as before).
func nodeless(pods []*corev1.Pod) bool {
	p := newestPod(pods)
	if p == nil {
		return false
	}
	switch p.Status.Phase {
	case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
		return false
	}
	return p.Spec.NodeName == "" && len(p.Status.ContainerStatuses) == 0 && len(p.Status.InitContainerStatuses) == 0
}

// scheduledAt is when the newest pod got its node (PodScheduled turned True); zero: not known.
func scheduledAt(pods []*corev1.Pod) time.Time {
	p := newestPod(pods)
	if p == nil {
		return time.Time{}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// nodeWaits are the sent pods (Starting) that wait for a node.
func nodeWaits(snap *clusterView, objs []*schedObject) map[*schedPod]bool {
	out := map[*schedPod]bool{}
	for _, o := range objs {
		for _, p := range o.pods {
			if p.state != laboratoryv1alpha1.PodStarting || p.ref == nil {
				continue
			}
			if nodeless(snap.podsOf[p.lookup]) {
				out[p] = true
			}
		}
	}
	return out
}

// commitKey is the gang of an object for the commit: its deploy group within its tenant; an independent lab and every LabGroup
// object are a gang of their own (as in planSchedule).
func commitKey(o *schedObject) string {
	if o.isGroup() || o.group == "" {
		return o.id
	}
	return "deploy/" + groupKey(objTenant(o), o.group)
}

// gangName names the gang of an object for people.
func gangName(o *schedObject) string {
	switch {
	case o.isGroup():
		return "lab group " + strings.TrimPrefix(o.id, "group/")
	case o.group != "":
		return "deploy group " + o.group
	}
	return "lab " + strings.TrimPrefix(o.id, "lab/")
}

// gangCommit is a gang whose commit failed: some pod of it has no node at the deadline.
type gangCommit struct {
	key string
	// objs are the objects withdrawn: the ones with a pod still starting or sent within the node timeout before the gang's last
	// dispatch (one commit); the labs of a deploy group that started long before are not part of it.
	objs     []*schedObject
	deadline time.Time
	noNode   []*schedPod
	// attempt is the number of this withdrawal in a row.
	attempt int32
}

// commitDue are the gangs whose deadline (their last dispatch + the node timeout) passed while some starting pod of theirs still
// waits for a node (waiting). A pod that was Ready once never makes its gang due: a running lab is never withdrawn.
func commitDue(objs []*schedObject, waiting map[*schedPod]bool, timeout time.Duration, now time.Time) []gangCommit {
	byKey := map[string][]*schedObject{}
	var keys []string
	for _, o := range objs {
		k := commitKey(o)
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], o)
	}
	sort.Strings(keys)
	var out []gangCommit
	for _, k := range keys {
		var latest time.Time
		var noNode []*schedPod
		for _, o := range byKey[k] {
			for _, p := range o.pods {
				if p.dispatched() && p.dispatchedAt.After(latest) {
					latest = p.dispatchedAt
				}
				if waiting[p] && p.state == laboratoryv1alpha1.PodStarting {
					noNode = append(noNode, p)
				}
			}
		}
		if len(noNode) == 0 || latest.IsZero() || now.Before(latest.Add(timeout)) {
			continue
		}
		g := gangCommit{key: k, deadline: latest.Add(timeout), noNode: noNode, attempt: 1}
		for _, o := range byKey[k] {
			member := false
			for _, p := range o.pods {
				if p.state == laboratoryv1alpha1.PodStarting || p.dispatched() && !p.dispatchedAt.Before(latest.Add(-timeout)) {
					member = true
				}
			}
			if !member {
				continue
			}
			g.objs = append(g.objs, o)
			for _, p := range o.pods {
				g.attempt = max(g.attempt, p.withdrawals+1)
			}
		}
		out = append(out, g)
	}
	return out
}

// withdrawDue withdraws every due gang and, with a static maximum, closes the breaker: admission places on the nodes that exist
// now only, until a new lab node becomes schedulable or one node timeout passes.
func (s *Scheduler) withdrawDue(ctx context.Context, snap *clusterView, due []gangCommit, now time.Time) {
	logger := log.FromContext(ctx).WithName("scheduler")
	for _, g := range due {
		s.withdraw(ctx, snap, g, now)
	}
	if s.Config.Maximum.Nodes <= 0 {
		return
	}
	var nodes corev1.NodeList
	if err := s.List(ctx, &nodes); err != nil {
		logger.Error(err, "list nodes for the capacity breaker")
		return
	}
	s.lastWithdrawal, s.breakerNodes = now, map[string]bool{}
	for i := range nodes.Items {
		if schedulableNode(&nodes.Items[i], s.LabNodeSelector, s.LabTolerations) {
			s.breakerNodes[nodes.Items[i].Name] = true
		}
	}
}

// withdraw sets every pod record of the gang's objects back to Queued (the dispatch, start and failure cleared, the attempt and
// the retry time set), the pods with a node first: when a write fails, a pod that waits for a node is still out, so the gang is
// due again on the next pass. The pass's view is updated in place. Each object gets one Warning event.
func (s *Scheduler) withdraw(ctx context.Context, snap *clusterView, g gangCommit, now time.Time) {
	logger := log.FromContext(ctx).WithName("scheduler")
	timeout := s.nodeTimeout()
	retry := now.Add(withdrawBackoff(g.attempt, timeout))
	waiting := map[*schedPod]bool{}
	for _, p := range g.noNode {
		waiting[p] = true
	}
	var pods []*schedPod
	for _, o := range g.objs {
		for _, p := range o.pods {
			if p.state != "" {
				pods = append(pods, p)
			}
		}
	}
	sort.SliceStable(pods, func(i, j int) bool { return !waiting[pods[i]] && waiting[pods[j]] })
	at := metav1.NewTime(retry)
	reset := func(ps *laboratoryv1alpha1.PodSchedule) {
		ps.State, ps.DispatchedAt, ps.StartedAt, ps.Failure = laboratoryv1alpha1.PodQueued, nil, nil, nil
		ps.Withdrawals, ps.RetryAfter = g.attempt, &at
	}
	for _, p := range pods {
		var err error
		switch p.kind {
		case kindDevicePod:
			d, ok := p.ref.(*laboratoryv1alpha1.Device)
			if !ok || d.Status.Scheduling == nil {
				continue
			}
			ps := d.Status.Scheduling.DeepCopy()
			reset(ps)
			err = s.setDevice(ctx, d, ps)
		case kindGroupPod:
			err = s.updateGroupPod(ctx, snap, p, reset)
		}
		if err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "withdraw pod", "pod", p.key)
			}
			continue
		}
		p.state, p.dispatchedAt, p.withdrawals, p.retryAfter = laboratoryv1alpha1.PodQueued, time.Time{}, g.attempt, retry
		delete(s.recent, p.key)
	}
	gangWithdrawals.Inc()
	logger.Info("withdrew a group: not every pod got a node within the node timeout", "group", g.key, "attempt", g.attempt, "retryAt", retry, "podsWithoutNode", len(g.noNode))
	if s.Recorder == nil {
		return
	}
	for _, o := range g.objs {
		if obj, ok := o.ref.(client.Object); ok {
			s.Recorder.Event(obj, corev1.EventTypeWarning, laboratoryv1alpha1.WaitWithdrawn, withdrawnMessage(timeout, g.attempt, retry))
		}
	}
}

// withdrawnMessage says why a group waits after a withdrawal.
func withdrawnMessage(timeout time.Duration, attempt int32, retry time.Time) string {
	return fmt.Sprintf("not every pod got a node within %s; withdrawn (attempt %d), retry at %s", timeout, attempt, retry.UTC().Format(time.RFC3339))
}

// updateGroupPod changes the scheduling record of a pod of a LabGroup.
func (s *Scheduler) updateGroupPod(ctx context.Context, snap *clusterView, p *schedPod, change func(*laboratoryv1alpha1.PodSchedule)) error {
	name := p.ref.(string)
	for _, g := range snap.groups {
		if "group/"+g.Name+"/"+name != p.key {
			continue
		}
		var ps laboratoryv1alpha1.PodSchedule
		for _, e := range g.Status.Pods {
			if e.Name == name {
				ps = e.PodSchedule
			}
		}
		change(&ps)
		return s.setGroupPod(ctx, g, name, ps)
	}
	return nil
}

// breakerOn says admission uses the nodes that exist only: a gang was withdrawn less than one node timeout ago and no lab node
// became schedulable since (one that was not schedulable then). Once it opens it is forgotten. A restart forgets it too, which
// only allows one more try.
func (s *Scheduler) breakerOn(nodes []corev1.Node, now time.Time) bool {
	if s.lastWithdrawal.IsZero() {
		return false
	}
	open := now.Sub(s.lastWithdrawal) >= s.nodeTimeout()
	for i := range nodes {
		if schedulableNode(&nodes[i], s.LabNodeSelector, s.LabTolerations) && !s.breakerNodes[nodes[i].Name] {
			open = true
		}
	}
	if open {
		s.lastWithdrawal, s.breakerNodes = time.Time{}, nil
		return false
	}
	return true
}

// nodeStarting is the NodeStarting status of an object whose pods are all sent and some of them wait for a node; ok false: none
// waits.
func nodeStarting(o *schedObject, waiting map[*schedPod]bool) (message string, ok bool) {
	n := 0
	var since time.Time
	for _, p := range o.pods {
		if !waiting[p] {
			continue
		}
		n++
		if since.IsZero() || p.dispatchedAt.Before(since) {
			since = p.dispatchedAt
		}
	}
	if n == 0 {
		return "", false
	}
	return fmt.Sprintf("%d pods wait for a node since %s", n, since.UTC().Format(time.RFC3339)), true
}

// reportNodeWaits sets the gauge of the pods waiting for a node and gives every object with such pods one Normal event; an
// object whose pods all have a node may get one again later.
func (s *Scheduler) reportNodeWaits(objs []*schedObject, waiting map[*schedPod]bool) {
	podsWaitingForNode.Set(float64(len(waiting)))
	if s.nodeStartingTold == nil {
		s.nodeStartingTold = map[types.UID]bool{}
	}
	now := map[types.UID]bool{}
	for _, o := range objs {
		obj, ok := o.ref.(client.Object)
		if !ok {
			continue
		}
		msg, waits := nodeStarting(o, waiting)
		if !waits {
			continue
		}
		now[obj.GetUID()] = true
		if s.nodeStartingTold[obj.GetUID()] {
			continue
		}
		s.nodeStartingTold[obj.GetUID()] = true
		if s.Recorder != nil {
			s.Recorder.Event(obj, corev1.EventTypeNormal, laboratoryv1alpha1.WaitNodeStarting, msg)
		}
	}
	for uid := range s.nodeStartingTold {
		if !now[uid] {
			delete(s.nodeStartingTold, uid)
		}
	}
}

// stuckGangs is, per tenant, the message of the GroupsWithdrawnRepeatedly condition: the gangs withdrawn repeatedWithdrawals
// times or more in a row (their pods were never Ready since), with their attempt.
func stuckGangs(objs []*schedObject, timeout time.Duration) map[string]string {
	attempts := map[string]map[string]int32{}
	for _, o := range objs {
		for _, p := range o.pods {
			if p.withdrawals < repeatedWithdrawals {
				continue
			}
			t := objTenant(o)
			if attempts[t] == nil {
				attempts[t] = map[string]int32{}
			}
			attempts[t][gangName(o)] = max(attempts[t][gangName(o)], p.withdrawals)
		}
	}
	out := map[string]string{}
	for t, gangs := range attempts {
		var list []string
		for name, n := range gangs {
			list = append(list, fmt.Sprintf("%s (attempt %d)", name, n))
		}
		sort.Strings(list)
		more := ""
		if len(list) > 10 {
			more = fmt.Sprintf(" and %d more", len(list)-10)
			list = list[:10]
		}
		out[t] = fmt.Sprintf("withdrawn %d times or more in a row, because not every pod got a node within %s: %s%s",
			repeatedWithdrawals, timeout, strings.Join(list, ", "), more)
	}
	return out
}
