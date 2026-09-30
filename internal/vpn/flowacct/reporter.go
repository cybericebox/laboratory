package flowacct

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

const (
	// ReportName is the LabTrafficReport written by the VPN pod. There is one
	// per group namespace.
	ReportName = "vpn"

	DefaultPollEvery   = 5 * time.Second
	DefaultReportEvery = time.Minute
)

// Reporter publishes the collector state as a LabTrafficReport. The custom
// resource is the delivery buffer: it survives a pod restart and an agent or
// platform outage, and the agent relays it without keeping any state itself.
type Reporter struct {
	Reader    client.Reader
	Writer    client.Client
	Namespace string
	Instance  string
	Collector *Collector
}

// Run polls the source and publishes on the given cadences until ctx ends.
// Collector failures are logged through onError and never stop the loop.
func (r *Reporter) Run(ctx context.Context, pollEvery, reportEvery time.Duration, onError func(error)) {
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()
	report := time.NewTicker(reportEvery)
	defer report.Stop()
	publish := func() {
		if err := r.Publish(ctx, time.Now()); err != nil && onError != nil {
			onError(err)
		}
	}
	if err := r.Collector.Poll(time.Now()); err != nil && onError != nil {
		onError(err)
	}
	publish()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			if err := r.Collector.Poll(time.Now()); err != nil && onError != nil {
				onError(err)
			}
		case <-report.C:
			publish()
		}
	}
}

// Publish writes the current state; CoveredTo advances even when nothing
// happened, which is what proves an idle team was being watched.
func (r *Reporter) Publish(ctx context.Context, now time.Time) error {
	key := types.NamespacedName{Namespace: r.Namespace, Name: ReportName}
	obj := &laboratoryv1alpha1.LabTrafficReport{}
	err := r.Reader.Get(ctx, key, obj)
	switch {
	case apierrors.IsNotFound(err):
		obj = &laboratoryv1alpha1.LabTrafficReport{
			ObjectMeta: metav1.ObjectMeta{Namespace: r.Namespace, Name: ReportName},
			Spec:       laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceVPN, Instance: r.Instance},
		}
		if err := r.Writer.Create(ctx, obj); err != nil {
			return fmt.Errorf("create traffic report: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get traffic report: %w", err)
	}
	obj.Status = ToStatus(r.Collector.Snapshot(now))
	if err := r.Writer.Status().Update(ctx, obj); err != nil {
		return fmt.Errorf("update traffic report status: %w", err)
	}
	return nil
}

// ToStatus converts a collector report to the custom resource status.
func ToStatus(report Report) laboratoryv1alpha1.LabTrafficReportStatus {
	status := laboratoryv1alpha1.LabTrafficReportStatus{
		BootID: report.BootID, CoveredFromMs: report.CoveredFromMs, CoveredToMs: report.CoveredToMs,
		Truncated: report.Truncated,
		Ledger:    make([]laboratoryv1alpha1.LabTrafficTouch, 0, len(report.Ledger)),
	}
	for _, t := range report.Ledger {
		status.Ledger = append(status.Ledger, laboratoryv1alpha1.LabTrafficTouch{
			Subject: t.Subject, LabName: t.Lab, DstIP: t.DstIP, Proto: t.Proto, DstPort: int32(t.DstPort),
			Attempts: t.Attempts, PacketsOut: t.PacketsOut, PacketsIn: t.PacketsIn,
			BytesOut: t.BytesOut, BytesIn: t.BytesIn,
			FirstSeenMs: t.FirstSeenMs, LastSeenMs: t.LastSeenMs, FirstRespondedMs: t.FirstRespondMs,
		})
	}
	return status
}
