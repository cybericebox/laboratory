package l7

import (
	"context"
	"regexp"
	"strings"
	"time"

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
	Reader     client.Reader
	Writer     client.Client
	Meter      *Meter
	Instance   string
	Namespaces func(ctx context.Context) []string
}

// Run publishes every interval until ctx ends, and once more on the way out.
func (w *ReportWriter) Run(ctx context.Context, every time.Duration, onError func(error)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			w.PublishAll(flush, time.Now(), onError)
			cancel()
			return
		case <-ticker.C:
			w.PublishAll(ctx, time.Now(), onError)
		}
	}
}

// PublishAll writes the report of every group namespace; a failure of one
// namespace (for example one that is being deleted) does not stop the others.
func (w *ReportWriter) PublishAll(ctx context.Context, now time.Time, onError func(error)) {
	for _, namespace := range w.Namespaces(ctx) {
		if err := w.Publish(ctx, namespace, now); err != nil && onError != nil {
			onError(err)
		}
	}
}

func (w *ReportWriter) Publish(ctx context.Context, namespace string, now time.Time) error {
	ledger, truncated := w.Meter.Ledger(namespace)
	status := laboratoryv1alpha1.LabTrafficReportStatus{
		BootID: w.Meter.BootID, CoveredFromMs: w.Meter.Started.UnixMilli(), CoveredToMs: now.UnixMilli(),
		Truncated: truncated, Ledger: make([]laboratoryv1alpha1.LabTrafficTouch, 0, len(ledger)),
	}
	for _, t := range ledger {
		status.Ledger = append(status.Ledger, laboratoryv1alpha1.LabTrafficTouch{
			Subject: t.Subject, LabName: t.Lab, Device: t.Device,
			Attempts: t.Attempts, BytesIn: t.BytesIn, BytesOut: t.BytesOut,
			FirstSeenMs: t.FirstSeenMs, LastSeenMs: t.LastSeenMs, FirstRespondedMs: t.RespondedMs,
		})
	}
	return flowacct.PublishReport(ctx, w.Reader, w.Writer,
		types.NamespacedName{Namespace: namespace, Name: ReportName(w.Instance)},
		laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceProxy, Instance: w.Instance},
		status)
}
