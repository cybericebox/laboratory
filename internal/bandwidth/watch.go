package bandwidth

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// AnnotationsDir and AnnotationsFile are where the pod's own annotations are mounted (a downward API volume).
const (
	AnnotationsDir  = "/etc/podinfo"
	AnnotationsFile = AnnotationsDir + "/annotations"
)

// ParseAnnotations reads the downward API file: one `key="value"` per line, the value in Go quoting.
func ParseAnnotations(data string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		key, quoted, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		if v, err := strconv.Unquote(quoted); err == nil {
			out[key] = v
		}
	}
	return out
}

// ReadLimits reads the limits annotation of the pod from the file; a missing file or annotation is no limits.
func ReadLimits(path string) (Limits, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Limits{}, nil
		}
		return Limits{}, err
	}
	return Decode(ParseAnnotations(string(data))[annotationKey])
}

// annotationKey is names.AnnotationBandwidthLimits; spelled here so the pods that watch it do not import the operator's packages.
const annotationKey = "cybericebox.com/bandwidth-limits"

// ConfigMapDir is where the chart mounts the ConfigMap of the entry limits (names.BandwidthConfigMap) into the demux and the L7 proxy.
const ConfigMapDir = "/etc/bandwidth"

// ReadLimitsFile reads limits written as JSON in a file (a key of the mounted ConfigMap); a missing file is no limits.
func ReadLimitsFile(path string) (Limits, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Limits{}, nil
		}
		return Limits{}, err
	}
	return Decode(string(data))
}

// Watch calls apply with the limits at the start and again each time they change, for a pod that reads them from its own annotations
// (the file AnnotationsFile): the gateway. A bad value calls onError and keeps the last good limits. It returns when ctx ends.
func Watch(ctx context.Context, path string, apply func(Limits), onError func(error)) error {
	return watchFile(ctx, filepath.Dir(path), func() (Limits, error) { return ReadLimits(path) }, apply, onError)
}

// WatchNode is Watch for a pod that reads the limits of its own node from the mounted ConfigMap: the file dir/node.
func WatchNode(ctx context.Context, dir, node string, apply func(Limits), onError func(error)) error {
	return watchFile(ctx, dir, func() (Limits, error) { return ReadLimitsFile(filepath.Join(dir, node)) }, apply, onError)
}

// watchFile watches the directory: the kubelet replaces the content of a volume by switching a symlink in it. A slow re-read (every
// minute) covers a lost event. Without the directory (a pod made before the volume existed) there is nothing to watch and nothing
// to apply: apply gets no limits once.
func watchFile(ctx context.Context, dir string, read func() (Limits, error), apply func(Limits), onError func(error)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := w.Add(dir); err != nil {
		apply(Limits{})
		<-ctx.Done()
		return nil
	}
	var last *Limits
	reread := func() {
		l, err := read()
		if err != nil {
			if onError != nil {
				onError(err)
			}
			return
		}
		if last == nil || *last != l {
			last = &l
			apply(l)
		}
	}
	reread()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.Events:
			time.Sleep(100 * time.Millisecond) // the kubelet writes several entries at once
			reread()
		case err := <-w.Errors:
			if onError != nil {
				onError(err)
			}
		case <-tick.C:
			reread()
		}
	}
}
