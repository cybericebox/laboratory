// Package activity keeps the raw activity facts one process of the laboratory hands, through the agent API, to the platform that runs it
// (the daemon): internet egress, console sessions, devices touched over the VPN. The facts sit in a bounded ring of a LabActivityReport in the
// group namespace; the agent reads them. The laboratory aggregates nothing and sends nothing anywhere by itself. Everything is off unless the
// chart value activityReports.enabled is true: a nil *Hub is the disabled state, and every method of it is a no-op.
package activity

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// The bounds of one writer's ring in one namespace.
const (
	MaxEvents = 1000
	MaxAge    = time.Hour
)

// FlushEvery is how often a writer looks for events to write: at most one write per ring per minute, and none when nothing is new.
const FlushEvery = time.Minute

// EnvEnabled is the variable every process reads (the chart sets it from activityReports.enabled).
const EnvEnabled = "ACTIVITY_REPORTS"

// Enabled reports whether this process was told to keep activity reports (chart value activityReports.enabled).
func Enabled() bool { return os.Getenv(EnvEnabled) == "true" }

var notDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// ReportName is the name of the LabActivityReport of a writer ("vpn", "egress", "proxy-<pod>").
func ReportName(writer string) string {
	name := strings.Trim(notDNS.ReplaceAllString(strings.ToLower(writer), "-"), "-")
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

type ring struct {
	loaded  bool
	dirty   bool
	seq     int64
	dropped int64
	events  []laboratoryv1alpha1.ActivityEvent
}

// Hub is the rings of one writer, one per group namespace.
type Hub struct {
	Client client.Client
	// Reader reads the report back before it is written (default: Client). Give an uncached reader where the process has no list and watch
	// right on the reports.
	Reader    client.Reader
	Writer    string
	MaxEvents int
	MaxAge    time.Duration
	Now       func() time.Time
	OnError   func(error)

	mu    sync.Mutex
	rings map[string]*ring
}

// New returns a Hub for a writer.
func New(c client.Client, writer string) *Hub {
	return &Hub{Client: c, Writer: writer, MaxEvents: MaxEvents, MaxAge: MaxAge, Now: time.Now, rings: map[string]*ring{}}
}

func (h *Hub) ringFor(namespace string) *ring {
	r := h.rings[namespace]
	if r == nil {
		// Sequence numbers start at the boot time in microseconds-ish units, so a restart never reuses one the platform may have seen.
		r = &ring{seq: h.Now().UnixMilli() * 1000}
		h.rings[namespace] = r
	}
	return r
}

func (h *Hub) reader() client.Reader {
	if h.Reader != nil {
		return h.Reader
	}
	return h.Client
}

// Add appends an event to the ring of a namespace. Seq is set here. A nil Hub (reports disabled) does nothing.
func (h *Hub) Add(namespace string, ev laboratoryv1alpha1.ActivityEvent) {
	if h == nil || namespace == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.ringFor(namespace)
	r.seq++
	ev.Seq = r.seq
	if ev.AtMs == 0 {
		ev.AtMs = h.Now().UnixMilli()
	}
	r.events = append(r.events, ev)
	r.dirty = true
	h.trim(r)
}

// trim keeps the ring within its bounds and remembers the highest Seq it dropped.
func (h *Hub) trim(r *ring) {
	cut := h.Now().Add(-h.MaxAge).UnixMilli()
	drop := 0
	for drop < len(r.events) && (len(r.events)-drop > h.MaxEvents || r.events[drop].AtMs < cut) {
		if r.events[drop].Seq > r.dropped {
			r.dropped = r.events[drop].Seq
		}
		drop++
	}
	if drop > 0 {
		r.events = append([]laboratoryv1alpha1.ActivityEvent(nil), r.events[drop:]...)
		r.dirty = true
	}
}

// Flush writes every ring that changed (or aged) to its LabActivityReport.
func (h *Hub) Flush(ctx context.Context) error {
	if h == nil {
		return nil
	}
	// Only a ring with something new to write costs an API call: an idle writer (and a group nobody does anything in, as during a lab
	// startup) makes none, not even a read.
	h.mu.Lock()
	var namespaces []string
	for ns, r := range h.rings {
		h.trim(r)
		if r.dirty {
			namespaces = append(namespaces, ns)
		}
	}
	h.mu.Unlock()
	var first error
	for _, ns := range namespaces {
		if err := h.flushOne(ctx, ns); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (h *Hub) flushOne(ctx context.Context, namespace string) error {
	name := ReportName(h.Writer)
	key := types.NamespacedName{Namespace: namespace, Name: name}
	h.mu.Lock()
	r := h.ringFor(namespace)
	loaded := r.loaded
	h.mu.Unlock()
	var obj laboratoryv1alpha1.LabActivityReport
	err := h.reader().Get(ctx, key, &obj)
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return err
	}
	h.mu.Lock()
	r = h.ringFor(namespace)
	if !loaded && !missing {
		// A restarted writer keeps what the platform has not pulled yet.
		r.events = append(append([]laboratoryv1alpha1.ActivityEvent(nil), obj.Status.Events...), r.events...)
		if obj.Status.DroppedThrough > r.dropped {
			r.dropped = obj.Status.DroppedThrough
		}
		if n := len(obj.Status.Events); n > 0 && obj.Status.Events[n-1].Seq > r.seq {
			r.seq = obj.Status.Events[n-1].Seq
		}
		r.dirty = true
	}
	r.loaded = true
	h.trim(r)
	if !r.dirty && !missing {
		h.mu.Unlock()
		return nil
	}
	status := laboratoryv1alpha1.LabActivityReportStatus{
		DroppedThrough: r.dropped, UpdatedMs: h.Now().UnixMilli(), Events: append([]laboratoryv1alpha1.ActivityEvent(nil), r.events...),
	}
	h.mu.Unlock()

	if missing {
		obj = laboratoryv1alpha1.LabActivityReport{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       laboratoryv1alpha1.LabActivityReportSpec{Writer: h.Writer},
		}
		if err := h.Client.Create(ctx, &obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s/%s: %w", namespace, name, err)
		} else if err != nil {
			if err := h.reader().Get(ctx, key, &obj); err != nil {
				return err
			}
		}
	}
	obj.Status = status
	if err := h.Client.Status().Update(ctx, &obj); err != nil {
		return fmt.Errorf("update %s/%s: %w", namespace, name, err)
	}
	h.mu.Lock()
	h.ringFor(namespace).dirty = false
	h.mu.Unlock()
	return nil
}

// Run flushes every interval until ctx ends, and once more at the end.
func (h *Hub) Run(ctx context.Context, every time.Duration) {
	if h == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	report := func(c context.Context) {
		if err := h.Flush(c); err != nil && h.OnError != nil {
			h.OnError(err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			report(final)
			cancel()
			return
		case <-t.C:
			report(ctx)
		}
	}
}

// Events returns a copy of the ring of a namespace (what the next flush writes).
func (h *Hub) Events(namespace string) []laboratoryv1alpha1.ActivityEvent {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rings[namespace]
	if r == nil {
		return nil
	}
	return append([]laboratoryv1alpha1.ActivityEvent(nil), r.events...)
}
