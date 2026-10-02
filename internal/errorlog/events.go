package errorlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The journal rides on Kubernetes Events, which every component may already write and the management agent may already read
// (the API server keeps them for an hour, deduplicates and rate-limits them): no log access and no new kind of permission.
const (
	// Reason of every journal event.
	Reason = "ErrorJournal"
	// Label marks the events of the journal (a cheap server-side selector for the reader).
	Label = "cybericebox.com/error-journal"

	annComponent   = "cybericebox.com/error-component"
	annInstance    = "cybericebox.com/error-instance"
	annFingerprint = "cybericebox.com/error-fingerprint"
	annKind        = "cybericebox.com/error-kind"
	annNormalized  = "cybericebox.com/error-normalized"
	annTotal       = "cybericebox.com/error-total"
	annFirst       = "cybericebox.com/error-first"
	annLast        = "cybericebox.com/error-last"
	annSamples     = "cybericebox.com/error-samples"
)

// Publisher writes the changed groups of an aggregator as Events.
type Publisher struct {
	Client    kubernetes.Interface
	Namespace string
	Agg       *Aggregator
	Interval  time.Duration
	Log       logr.Logger
}

// Run publishes every Interval (30 s by default) until ctx ends, and once more at the end.
func (p *Publisher) Run(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			p.Flush(flush)
			cancel()
			return
		case <-t.C:
			p.Flush(ctx)
		}
	}
}

// Flush publishes the groups that changed since the last flush. A failure to publish is not an error of the journal (it would
// feed itself): the group is marked changed again and tried at the next flush.
func (p *Publisher) Flush(ctx context.Context) {
	for _, g := range p.Agg.Dirty() {
		if err := p.publish(ctx, g); err != nil {
			p.Agg.remark(g.Fingerprint)
			if p.Log.GetSink() != nil {
				p.Log.V(1).Info("error journal: publish failed", "err", err.Error())
			}
		}
	}
}

func (a *Aggregator) remark(fp string) {
	a.mu.Lock()
	a.dirty[fp] = true
	a.mu.Unlock()
}

func eventName(component, instance, fp string) string {
	sum := sha256.Sum256([]byte(component + "\x00" + instance))
	return "errj-" + hex.EncodeToString(sum[:4]) + "-" + fp
}

func (p *Publisher) publish(ctx context.Context, g Group) error {
	samples, _ := json.Marshal(g.Samples)
	ann := map[string]string{
		annComponent: p.Agg.Component, annInstance: p.Agg.Instance, annFingerprint: g.Fingerprint, annKind: g.Kind,
		annNormalized: g.Normalized, annTotal: strconv.FormatInt(g.Total, 10),
		annFirst: strconv.FormatInt(g.First.UnixMilli(), 10), annLast: strconv.FormatInt(g.Last.UnixMilli(), 10), annSamples: string(samples),
	}
	count := int32(g.Total)
	if g.Total > math.MaxInt32 {
		count = math.MaxInt32
	}
	name := eventName(p.Agg.Component, p.Agg.Instance, g.Fingerprint)
	msg := g.Normalized
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	last := metav1.NewTime(g.Last)
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace, Labels: map[string]string{Label: "true"}, Annotations: ann},
		// The object is the component's own pod; it need not exist (an event of a pod that has gone is still readable).
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", APIVersion: "v1", Name: p.Agg.Instance, Namespace: p.Namespace},
		Reason:         Reason, Message: msg, Type: corev1.EventTypeWarning,
		Source: corev1.EventSource{Component: p.Agg.Component, Host: p.Agg.Instance}, Count: count,
		FirstTimestamp: metav1.NewTime(g.First), LastTimestamp: last,
	}
	_, err := p.Client.CoreV1().Events(p.Namespace).Create(ctx, ev, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		patch, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"annotations": ann}, "count": count, "message": msg, "lastTimestamp": last,
		})
		_, err = p.Client.CoreV1().Events(p.Namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	}
	return err
}

// Observation is one journal event read back.
type Observation struct {
	Component, Instance, Fingerprint, Kind, Normalized string
	Total                                              int64
	First, Last                                        time.Time
	Samples                                            []string
}

// ParseEvent reads a journal event; false when it is not one (or is damaged).
func ParseEvent(ev *corev1.Event) (Observation, bool) {
	if ev.Reason != Reason || ev.Labels[Label] != "true" {
		return Observation{}, false
	}
	a := ev.Annotations
	total, err := strconv.ParseInt(a[annTotal], 10, 64)
	if err != nil || a[annFingerprint] == "" || a[annComponent] == "" {
		return Observation{}, false
	}
	ms := func(s string) time.Time {
		n, _ := strconv.ParseInt(s, 10, 64)
		return time.UnixMilli(n)
	}
	o := Observation{Component: a[annComponent], Instance: a[annInstance], Fingerprint: a[annFingerprint], Kind: a[annKind],
		Normalized: a[annNormalized], Total: total, First: ms(a[annFirst]), Last: ms(a[annLast])}
	_ = json.Unmarshal([]byte(a[annSamples]), &o.Samples)
	// The reader never trusts what it reads: it redacts again, and bounds what it keeps.
	o.Normalized = Redact(o.Normalized)
	for i := range o.Samples {
		o.Samples[i] = Redact(o.Samples[i])
	}
	if len(o.Samples) > MaxSamples {
		o.Samples = o.Samples[len(o.Samples)-MaxSamples:]
	}
	return o, true
}

// String is for logs and tests.
func (o Observation) String() string {
	return fmt.Sprintf("%s/%s %s x%d %q", o.Component, o.Instance, o.Fingerprint, o.Total, o.Normalized)
}

// Setup wraps a logger so that its errors are journalled; the returned aggregator is what Start publishes.
func Setup(component, instance string, base logr.Logger) (logr.Logger, *Aggregator) {
	agg := New(component, instance)
	return WrapLogger(base, agg), agg
}

// Instance is the name of this process's pod (its hostname), never an address.
func Instance() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

// Start publishes the aggregator's changed groups as Events in namespace until ctx ends. The journal is best effort: a cluster
// that will not take the events only loses the journal, the component is never stopped for it.
func Start(ctx context.Context, agg *Aggregator, cfg *rest.Config, namespace string, log logr.Logger) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.V(1).Info("error journal: no client", "err", err.Error())
		return
	}
	go (&Publisher{Client: cs, Namespace: namespace, Agg: agg, Log: log}).Run(ctx)
}

// Namespace is where this component publishes: ERROR_JOURNAL_NAMESPACE, else the default.
func Namespace(def string) string {
	if v := os.Getenv("ERROR_JOURNAL_NAMESPACE"); v != "" {
		return v
	}
	return def
}
