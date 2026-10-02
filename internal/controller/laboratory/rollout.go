package laboratory

import (
	"context"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// rolloutPoll is how often a group waiting for its turn, or rolling, is looked at again.
	rolloutPoll = 10 * time.Second
	// rolloutStale is how long one group may hold the turn: a group whose new pods never become ready does not hold up every other group forever.
	rolloutStale = 10 * time.Minute
)

// rolloutGuard lets one group at a time have its VPN and gateway pods replaced because the configured image, command or resources
// changed (a chart upgrade): the pods of a group restart together, so every group at once would drop every tunnel of the event at once.
type rolloutGuard struct {
	mu      sync.Mutex
	holder  string
	since   time.Time
	waiting map[string]bool
	now     func() time.Time
}

func (g *rolloutGuard) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// begin starts a reconcile of a group: it has not been told to wait yet.
func (g *rolloutGuard) begin(ns string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.waiting, ns)
}

// try says whether the group may roll now: it holds the turn, or nobody does (or the holder's turn went stale). A group that may not is
// remembered as waiting.
func (g *rolloutGuard) try(ns string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if g.holder == "" || g.holder == ns || now.Sub(g.since) > rolloutStale {
		if g.holder != ns {
			g.holder, g.since = ns, now
		}
		return true
	}
	if g.waiting == nil {
		g.waiting = map[string]bool{}
	}
	g.waiting[ns] = true
	return false
}

func (g *rolloutGuard) holds(ns string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holder == ns
}

func (g *rolloutGuard) isWaiting(ns string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiting[ns]
}

func (g *rolloutGuard) release(ns string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.holder == ns {
		g.holder = ""
	}
}

// pullPolicyFor is the image pull policy that follows the tag: a tag that moves (latest, or none) is pulled every time, an exact tag or a
// digest only when the node does not have it.
func pullPolicyFor(image string) corev1.PullPolicy {
	if strings.Contains(image, "@") {
		return corev1.PullIfNotPresent
	}
	name := image[strings.LastIndex(image, "/")+1:]
	tag := ""
	if i := strings.LastIndex(name, ":"); i >= 0 {
		tag = name[i+1:]
	}
	if tag == "" || tag == "latest" {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

// convergeGroupPod brings the container of an existing VPN or gateway Deployment to the configured image, command, pull policy and
// resources. A difference is a rolling update of the group's pod, so it is applied only when the group has the turn (see rolloutGuard);
// otherwise it is left for a later reconcile. It reports whether it changed the Deployment.
func (r *LabGroupReconciler) convergeGroupPod(ctx context.Context, ns string, d *appsv1.Deployment, container, image string, command []string, resources corev1.ResourceRequirements) bool {
	var c *corev1.Container
	for i := range d.Spec.Template.Spec.Containers {
		if d.Spec.Template.Spec.Containers[i].Name == container {
			c = &d.Spec.Template.Spec.Containers[i]
		}
	}
	if c == nil {
		return false
	}
	if image == "" { // not configured (tests): nothing to converge to
		return false
	}
	want := r.cachedImage(ctx, ns, image)
	policy := pullPolicyFor(want)
	if c.Image == want && c.ImagePullPolicy == policy && apiequality.Semantic.DeepEqual(c.Command, command) && apiequality.Semantic.DeepEqual(c.Resources, resources) {
		return false
	}
	if !r.rollout.try(ns) {
		return false
	}
	c.Image, c.ImagePullPolicy, c.Command, c.Resources = want, policy, command, resources
	return true
}

// settleRollout runs after both Deployments of a group were ensured. It releases the group's turn when its pods are rolled out, and says
// whether the group must be looked at again soon (it waits for its turn, or is rolling).
func (r *LabGroupReconciler) settleRollout(ctx context.Context, ns string) (bool, error) {
	if r.rollout.isWaiting(ns) {
		return true, nil
	}
	if !r.rollout.holds(ns) {
		return false, nil
	}
	for _, name := range []string{"vpn", "gateway"} {
		var d appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &d); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if !rolledOut(&d) {
			return true, nil
		}
	}
	r.rollout.release(ns)
	return false, nil
}

// rolledOut says whether a Deployment runs its latest template everywhere and all of it is available.
func rolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.UpdatedReplicas == want && s.Replicas == want && s.AvailableReplicas == want
}
