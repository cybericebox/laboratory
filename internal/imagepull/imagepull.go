// Package imagepull pulls images onto a node through the container runtime's image
// service (CRI), for the node-agent. Nothing from an image runs: the runtime only fetches
// and unpacks it, which is all a prepull needs, and the credentials of the pull are the
// tenant's own, handed over per request.
package imagepull

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

// Auth is the registry credential of one pull.
type Auth struct {
	Username, Password string
}

// Puller is the container runtime's image service.
type Puller interface {
	// Present reports whether the node already holds the image.
	Present(ctx context.Context, ref string) (bool, error)
	// Pull fetches the image; auth is nil for an anonymous pull.
	Pull(ctx context.Context, ref string, auth *Auth) error
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
	// Credentials returns the credential for an image's registry; nil or a nil Auth means anonymous.
	Credentials func(ref string) *Auth
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
			var auth *Auth
			if o.Credentials != nil {
				auth = o.Credentials(img)
			}
			err := pullOne(ctx, p, img, auth, o.Timeout)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Failed = append(res.Failed, Failure{Image: img, Message: scrub(err.Error(), auth)})
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

func pullOne(ctx context.Context, p Puller, ref string, auth *Auth, timeout time.Duration) error {
	if _, err := name.ParseReference(ref); err != nil {
		return fmt.Errorf("invalid image reference: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if ok, err := p.Present(ctx, ref); err == nil && ok {
		return nil
	}
	return p.Pull(ctx, ref, auth)
}

// scrub keeps a runtime message short and free of the credential it was given.
func scrub(msg string, auth *Auth) string {
	if auth != nil {
		for _, secret := range []string{auth.Password, auth.Username} {
			if len(secret) >= 4 {
				msg = strings.ReplaceAll(msg, secret, "***")
			}
		}
	}
	if len(msg) > maxMessage {
		msg = msg[:maxMessage]
	}
	return msg
}
