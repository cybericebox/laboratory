package l7

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

const reportNamePrefix = "proxy-"

var notDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// ReportName is the LabTrafficReport of one proxy replica.
func ReportName(instance string) string {
	name := notDNS.ReplaceAllString(strings.ToLower(instance), "-")
	name = strings.Trim(name, "-")
	if len(name) > 200 {
		name = name[:200]
	}
	return reportNamePrefix + name
}

// ReportWriter publishes what the Meter counted, one LabTrafficReport per proxy
// replica in every group namespace, every interval. The covered span advances
// on each write even when the group saw no request, which is how an idle team
// proves it was being watched. The agent relays the reports to the platform.
type ReportWriter struct {
	mu         sync.Mutex
	readyMu    sync.RWMutex
	prepared   map[string]bool
	Reader     client.Reader
	Writer     client.Client
	Meter      *Meter
	Instance   string
	Namespaces func(ctx context.Context) []string
}

// Run publishes periodically. The server lifecycle owns the final write after requests settle.
func (w *ReportWriter) Run(ctx context.Context, every time.Duration, onError func(error)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.PublishAll(ctx, time.Now(), onError)
		}
	}
}

// PublishAll writes the report of every group namespace; a failure of one
// namespace (for example one that is being deleted) does not stop the others.
func (w *ReportWriter) PublishAll(ctx context.Context, now time.Time, onError func(error)) {
	namespaces := w.Namespaces(ctx)
	for _, namespace := range namespaces {
		if err := w.Publish(ctx, namespace, now); err != nil && onError != nil {
			onError(err)
		}
	}
	// A nil inventory means the cache read failed; it cannot retire anything.
	if namespaces != nil {
		active := make(map[string]bool, len(namespaces))
		for _, ns := range namespaces {
			active[ns] = true
		}
		w.mu.Lock()
		w.Meter.RetireNamespaces(active)
		w.readyMu.Lock()
		for ns := range w.prepared {
			if !active[ns] {
				delete(w.prepared, ns)
			}
		}
		w.readyMu.Unlock()
		w.mu.Unlock()
	}

}

// Prepare restores the previous report before a namespace can admit traffic.
// Successful preparation is a cheap map lookup; failures remain retryable and
// cannot publish smaller counters over the durable baseline.
func (w *ReportWriter) Prepare(ctx context.Context, namespace string) error {
	w.readyMu.RLock()
	ready := w.prepared[namespace]
	w.readyMu.RUnlock()
	if ready {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.prepareLocked(ctx, namespace)
}
func (w *ReportWriter) prepareLocked(ctx context.Context, namespace string) error {
	w.readyMu.RLock()
	ready := w.prepared[namespace]
	w.readyMu.RUnlock()
	if ready {
		return nil
	}
	var old laboratoryv1alpha1.LabTrafficReport
	if err := w.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ReportName(w.Instance)}, &old); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
	} else if old.Spec.Kind != laboratoryv1alpha1.LabTrafficSurfaceProxy {
		return fmt.Errorf("existing traffic report belongs to another collector")
	}
	rows := make([]Touch, 0, len(old.Status.Ledger))
	for _, t := range old.Status.Ledger {
		rows = append(rows, Touch{Subject: t.Subject, Lab: t.LabName, Attempts: t.Attempts, BytesIn: t.BytesIn, BytesOut: t.BytesOut, FirstSeenMs: t.FirstSeenMs, LastSeenMs: t.LastSeenMs, RespondedMs: t.FirstRespondedMs})
	}
	w.Meter.Restore(namespace, rows, old.Status.Truncated, old.Status.Partial)
	w.readyMu.Lock()
	if w.prepared == nil {
		w.prepared = map[string]bool{}
	}
	w.prepared[namespace] = true
	w.readyMu.Unlock()
	return nil
}
func (w *ReportWriter) PrepareAll(ctx context.Context) error {
	for _, namespace := range w.Namespaces(ctx) {
		if err := w.Prepare(ctx, namespace); err != nil {
			return err
		}
	}
	return nil
}

func (w *ReportWriter) Publish(ctx context.Context, namespace string, now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.prepareLocked(ctx, namespace); err != nil {
		return err
	}
	ledger, truncated, partial := w.Meter.Snapshot(namespace)
	status := laboratoryv1alpha1.LabTrafficReportStatus{
		BootID: w.Meter.BootID, CoveredFromMs: w.Meter.Started.UnixMilli(), CoveredToMs: now.UnixMilli(),
		Partial: partial, Truncated: truncated, Ledger: make([]laboratoryv1alpha1.LabTrafficTouch, 0, len(ledger)),
	}
	for _, t := range ledger {
		status.Ledger = append(status.Ledger, laboratoryv1alpha1.LabTrafficTouch{
			Subject: t.Subject, LabName: t.Lab,
			Attempts: t.Attempts, BytesIn: t.BytesIn, BytesOut: t.BytesOut,
			FirstSeenMs: t.FirstSeenMs, LastSeenMs: t.LastSeenMs, FirstRespondedMs: t.RespondedMs,
		})
	}
	return flowacct.PublishReport(ctx, w.Reader, w.Writer,
		types.NamespacedName{Namespace: namespace, Name: ReportName(w.Instance)},
		laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceProxy, Instance: w.Instance},
		status)
}

// LogReportFailure is the onError of Run: a refusal (Forbidden) or NotFound is how a group namespace looks that the operator has
// not bound the proxy's role in yet, or that is being deleted. Both pass by themselves, so they are only traced (debug), never
// an error in the journal.
func LogReportFailure(log logr.Logger) func(error) {
	return func(err error) {
		if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
			log.V(1).Info("traffic report skipped: the group namespace is not ready for it yet or is going away", "reason", err.Error())
			return
		}
		log.Error(err, "publish traffic report")
	}
}
