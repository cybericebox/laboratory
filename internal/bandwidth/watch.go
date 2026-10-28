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

// Watch calls apply with the limits at the start and again each time the file changes. The kubelet replaces the volume content by
// switching a symlink in the directory, so the directory is watched. A bad value calls onError and keeps the last good limits. It
// returns when ctx ends. A slow re-read (every minute) covers a lost event.
func Watch(ctx context.Context, path string, apply func(Limits), onError func(error)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		// no volume (a pod made before the limits existed): nothing to watch, nothing to apply
		apply(Limits{})
		<-ctx.Done()
		return nil
	}
	var last *Limits
	read := func() {
		l, err := ReadLimits(path)
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
	read()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.Events:
			time.Sleep(100 * time.Millisecond) // the kubelet writes several entries at once
			read()
		case err := <-w.Errors:
			if onError != nil {
				onError(err)
			}
		case <-tick.C:
			read()
		}
	}
}
