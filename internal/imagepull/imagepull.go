// Package imagepull pulls images onto a node through the container runtime's image
// service (CRI), for the node-agent. Nothing from an image runs: the runtime only fetches
// and unpacks it, which is all a prepull needs. The pull carries no credentials: the references already
// name the tenant (<prefix>/c/<tenant>/...), and the registry gate decides by the node and the reference.
package imagepull

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Puller is the container runtime's image service.
type Puller interface {
	// Present reports whether the node already holds the image.
	Present(ctx context.Context, ref string) (bool, error)
	// Pull fetches the image.
	Pull(ctx context.Context, ref string) error
}

// Failure is an image that could not be pulled.
type Failure struct {
	Image, Message string
}

// Result is the outcome for one node: every image is in exactly one of the lists.
type Result struct {
	Pulled []string
	Failed []Failure
}

// Options bound a Pull.
type Options struct {
	// Concurrency is how many images are pulled at once (2).
	Concurrency int
	// Timeout bounds one image (5m).
	Timeout time.Duration
}

const maxMessage = 300

// Pull makes the node hold the images. An image the node already holds is not pulled
// again; one that cannot be pulled (a wrong name or tag, no access) is reported at once
// with the runtime's message, the others carry on. Both lists are sorted.
func Pull(ctx context.Context, p Puller, images []string, o Options) Result {
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	var (
		mu  sync.Mutex
		res Result
		wg  sync.WaitGroup
		sem = make(chan struct{}, o.Concurrency)
	)
	seen := map[string]bool{}
	for _, img := range images {
		if img == "" || seen[img] {
			continue
		}
		seen[img] = true
		wg.Add(1)
		sem <- struct{}{}
		go func(img string) {
			defer wg.Done()
			defer func() { <-sem }()
			err := pullOne(ctx, p, img, o.Timeout)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Failed = append(res.Failed, Failure{Image: img, Message: scrub(err.Error())})
				return
			}
			res.Pulled = append(res.Pulled, img)
		}(img)
	}
	wg.Wait()
	sort.Strings(res.Pulled)
	sort.Slice(res.Failed, func(i, j int) bool { return res.Failed[i].Image < res.Failed[j].Image })
	return res
}

func pullOne(ctx context.Context, p Puller, ref string, timeout time.Duration) error {
	if _, err := name.ParseReference(ref); err != nil {
		return fmt.Errorf("invalid image reference: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	if ok, err := p.Present(ctx, ref); err == nil && ok {
		pullSeconds.WithLabelValues("present").Observe(time.Since(started).Seconds())
		return nil
	}
	err := p.Pull(ctx, ref)
	result := "pulled"
	if err != nil {
		result = "error"
	}
	pullSeconds.WithLabelValues(result).Observe(time.Since(started).Seconds())
	return err
}

// pullSeconds is the time to make the node hold one image (no image name is a label). result: present (it was there already), pulled
// or error.
var pullSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "laboratory_image_pull_seconds",
	Help:    "Time to make the node hold one image for a prepull request. result: present, pulled or error.",
	Buckets: []float64{0.05, 0.25, 1, 2, 5, 10, 20, 30, 60, 120, 300},
}, []string{"result"})

func init() { metrics.Registry.MustRegister(pullSeconds) }

// scrub keeps a runtime message short.
func scrub(msg string) string {
	if len(msg) > maxMessage {
		msg = msg[:maxMessage]
	}
	return msg
}
