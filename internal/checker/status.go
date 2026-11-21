package checker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// StatusWriter writes the results into Lab.status with server-side apply, so that it cannot fight with the lab controller's own
// status writes (which are plain updates of other fields):
//
//   - one apply carries one entry of status.checks (the list is a map keyed by id), under a field manager of its own for that
//     check, so applying an entry never touches another entry, any other field of the status, or the fields the controller owns;
//   - the controller's Status().Update of the whole object does not take over or drop the entries: an update only changes the
//     ownership of fields whose value it changes, and it never changes these;
//   - status.checksRunAck is applied under a manager of its own, the same for every node.
//
// The applied object is unstructured, never the typed Lab: a typed object serializes every non-omitempty field (empty structs
// such as vpn and internet), and an apply would then claim them.
type StatusWriter struct {
	Client client.Client
}

// ManagerForCheck is the field manager of one check: fixed length and characters whatever the id is.
func ManagerForCheck(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "lab-checker." + hex.EncodeToString(sum[:8])
}

// AckManager is the field manager of status.checksRunAck.
const AckManager = "lab-checker-ack"

func labObject(lab LabKey, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": laboratoryv1alpha1.SchemeGroupVersion.String(),
		"kind":       "Lab",
		"metadata":   map[string]any{"name": lab.Name, "namespace": lab.Namespace},
		"status":     status,
	}}
}

// resultObject is the apply configuration of one check result. Empty optional fields are left out, so that an apply that no longer
// has a value or a detail removes it.
func resultObject(lab LabKey, r laboratoryv1alpha1.CheckResult) *unstructured.Unstructured {
	entry := map[string]any{"id": r.ID, "state": string(r.State)}
	if r.FirstPassedMs != 0 {
		entry["firstPassedMs"] = r.FirstPassedMs
	}
	if r.ChangedMs != 0 {
		entry["changedMs"] = r.ChangedMs
	}
	if r.Value != "" {
		entry["value"] = r.Value
	}
	if r.Detail != "" {
		entry["detail"] = r.Detail
	}
	return labObject(lab, map[string]any{"checks": []any{entry}})
}

// PublishCheck applies one entry of status.checks.
func (w *StatusWriter) PublishCheck(ctx context.Context, lab LabKey, r laboratoryv1alpha1.CheckResult) error {
	err := w.Client.SubResource("status").Apply(ctx, client.ApplyConfigurationFromUnstructured(resultObject(lab, r)), client.FieldOwner(ManagerForCheck(r.ID)), client.ForceOwnership)
	if err != nil {
		return fmt.Errorf("apply check %q of %s: %w", r.ID, lab, err)
	}
	return nil
}

// AckRun applies status.checksRunAck.
func (w *StatusWriter) AckRun(ctx context.Context, lab LabKey, token int64) error {
	obj := labObject(lab, map[string]any{"checksRunAck": token})
	if err := w.Client.SubResource("status").Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner(AckManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("acknowledge run %d of %s: %w", token, lab, err)
	}
	return nil
}
