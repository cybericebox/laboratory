package laboratory

import (
	"context"
	"encoding/json"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

// retireRetained deletes registry manifests at explicit accepted expiry, leaving
// the Lab definition and history readable. Backend owns future-stage dependency
// protection: it must withhold/extend deadlines while those copies are needed.
// A metadata resourceVersion CAS claims retirement before deleting a manifest;
// StartLabs observes this fence, so a new start and retirement cannot both win.
func (s *RetentionSweeper) retireRetained(ctx context.Context, reader client.Reader, l *lab.Lab, repos []string, now time.Time) error {
	intent := l.Spec.Lifecycle
	if intent == nil || !intent.IsStopped() || intent.RetentionUntil == nil || now.Before(intent.RetentionUntil.Time) || !l.DeletionTimestamp.IsZero() {
		return nil
	}
	if !retirementReady(l) {
		return nil
	}
	f, ok := lab.ParseSnapshotRetirement(l.Annotations[names.AnnotationSnapshotRetirement])
	if !ok {
		if l.Annotations[names.AnnotationSnapshotRetirement] != "" {
			return fmt.Errorf("invalid snapshot retirement fence for %s/%s", l.Namespace, l.Name)
		}
		f = lab.SnapshotRetirementFence{LabUID: string(l.UID), OperationID: intent.OperationID, Revision: intent.Revision, State: "DeleteRequested"}
		if err := s.writeRetirement(ctx, l, f); err != nil {
			return err
		}
	} else if f.LabUID != string(l.UID) || f.OperationID != intent.OperationID || f.Revision != intent.Revision {
		return fmt.Errorf("snapshot retirement identity changed")
	}
	if f.State == "Deleted" {
		return nil
	}
	var deletionErr error
	for _, repo := range repos {
		var live lab.Lab
		if err := reader.Get(ctx, client.ObjectKeyFromObject(l), &live); err != nil {
			return err
		}
		current, valid := lab.ParseSnapshotRetirement(live.Annotations[names.AnnotationSnapshotRetirement])
		if !valid || current.LabUID != f.LabUID || current.OperationID != f.OperationID || current.Revision != f.Revision || !retirementReady(&live) {
			return fmt.Errorf("snapshot retirement was superseded")
		}
		if err := s.Registry.DeleteRepo(ctx, repo); err != nil {
			deletionErr = err
		}
	}
	var live lab.Lab
	if err := reader.Get(ctx, client.ObjectKeyFromObject(l), &live); err != nil {
		return err
	}
	current, valid := lab.ParseSnapshotRetirement(live.Annotations[names.AnnotationSnapshotRetirement])
	if !valid || current.LabUID != f.LabUID || current.OperationID != f.OperationID || current.Revision != f.Revision {
		return fmt.Errorf("snapshot retirement was superseded")
	}
	f.State = "Deleted"
	if deletionErr != nil {
		f.State = "CleanupPending"
	}
	if err := s.writeRetirement(ctx, &live, f); err != nil {
		return err
	}
	return deletionErr
}
func retirementReady(l *lab.Lab) bool {
	i, o, a := l.Spec.Lifecycle, l.Status.Lifecycle, l.Status.Resources
	if i == nil || !i.IsStopped() || o == nil || a == nil {
		return false
	}
	return o.LabUID == string(l.UID) && o.OperationID == i.OperationID && o.Revision == i.Revision && o.ObservedGeneration == l.Generation && o.ObservedState == "Stopped" && nonzeroTime(o.StoppedAt) && a.OperationID == i.OperationID && a.Revision == i.Revision && a.RuntimeState == "Released" && nonzeroTime(a.ReleasedAt) && nonzeroTime(a.ObservedAt) && a.AllocatedRequests == (lab.ResourceAmounts{}) && (i.SnapshotMode != "Required" || o.SnapshotComplete && o.Error == "") && (!l.Spec.VPN.Enabled || o.AccessFenced && nonzeroTime(o.AccessFencedAt) && o.AccessFenceVPNBootID != "")
}
func (s *RetentionSweeper) writeRetirement(ctx context.Context, l *lab.Lab, f lab.SnapshotRetirementFence) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if l.Annotations == nil {
		l.Annotations = map[string]string{}
	}
	l.Annotations[names.AnnotationSnapshotRetirement] = string(raw)
	return s.Client.Update(ctx, l)
}
