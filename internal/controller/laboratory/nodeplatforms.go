package laboratory

import (
	"context"
	"sort"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodePlatforms returns a function listing the distinct OS/architecture pairs
// of the nodes lab pods may run on (those matching the lab node selector).
// Lab nodes come and go with the autoscaler: with none, the last pairs seen are
// kept, and before any were seen the pairs of every Linux node stand in, so an
// image is not pinned to its index while the lab nodes are at zero and then
// re-pinned to the platform manifest (a template change that restarts every
// group pod) when the first one joins.
func NodePlatforms(reader client.Reader, nodeSelector map[string]string) func(context.Context) []v1.Platform {
	var mu sync.Mutex
	var last []v1.Platform
	return func(ctx context.Context) []v1.Platform {
		var nodes corev1.NodeList
		if err := reader.List(ctx, &nodes, client.MatchingLabels(nodeSelector)); err != nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if got := platformsOf(nodes.Items); len(got) > 0 {
			last = got
			return got
		}
		if len(last) > 0 {
			return last
		}
		var all corev1.NodeList
		if err := reader.List(ctx, &all, client.MatchingLabels{corev1.LabelOSStable: "linux"}); err != nil {
			return nil
		}
		return platformsOf(all.Items)
	}
}

func platformsOf(nodes []corev1.Node) []v1.Platform {
	seen := map[string]v1.Platform{}
	for _, n := range nodes {
		os, arch := n.Labels[corev1.LabelOSStable], n.Labels[corev1.LabelArchStable]
		if os == "" || arch == "" {
			continue
		}
		seen[os+"/"+arch] = v1.Platform{OS: os, Architecture: arch}
	}
	out := make([]v1.Platform, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Architecture < out[j].Architecture })
	return out
}
