package grpc

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	controller "github.com/cybericebox/laboratory/internal/controller/laboratory"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// StopLabs accepts durable intent only. Completion requires matching observations.
func (h *Handler) StopLabs(ctx context.Context, in *protobuf.StopLabsRequest) (*protobuf.BatchResult, error) {
	if err := checkLifecycleCount(len(in.GetItems())); err != nil {
		return nil, err
	}
	refs := make([]*protobuf.ItemRef, len(in.GetItems()))
	for i, it := range in.GetItems() {
		refs[i] = it.GetTarget().GetRef()
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	resolver := h.newResolver(ctx)
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		it := in.Items[i]
		target := it.GetTarget()
		intent := &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: target.GetOperationId(), Revision: target.GetLifecycleRevision(), Terminal: it.GetTerminal()}
		switch it.GetSnapshotMode() {
		case protobuf.StopSnapshotMode_STOP_SNAPSHOT_MODE_SKIP:
			intent.SnapshotMode = "Skip"
		case protobuf.StopSnapshotMode_STOP_SNAPSHOT_MODE_REQUIRED:
			intent.SnapshotMode = "Required"
		default:
			return failedResult(refs[i], fmt.Errorf("snapshot_mode must be explicitly Skip or Required"))
		}
		if it.GetRetentionUntilUnixMs() < 0 {
			return failedResult(refs[i], fmt.Errorf("retention deadline cannot be negative"))
		}
		if it.GetRetentionUntilUnixMs() > 0 {
			// metav1.Time serializes whole seconds. Round UP to avoid retiring
			// before the explicit millisecond deadline and canonicalize retries.
			deadline := time.UnixMilli(it.GetRetentionUntilUnixMs()).UTC()
			whole := deadline.Truncate(time.Second)
			if deadline.After(whole) {
				whole = whole.Add(time.Second)
			}
			t := metav1.NewTime(whole)
			intent.RetentionUntil = &t
		}
		if err := h.acceptLifecycle(ctx, resolver, target, intent); err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}

// StartLabs never clears terminal intent or changes immutable runtime identity.
func (h *Handler) StartLabs(ctx context.Context, in *protobuf.StartLabsRequest) (*protobuf.BatchResult, error) {
	if err := checkLifecycleCount(len(in.GetItems())); err != nil {
		return nil, err
	}
	refs := make([]*protobuf.ItemRef, len(in.GetItems()))
	for i, t := range in.GetItems() {
		refs[i] = t.GetRef()
	}
	if err := dupRefs(refs); err != nil {
		return nil, err
	}
	resolver := h.newResolver(ctx)
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		t := in.Items[i]
		intent := &lab.LabLifecycleSpec{DesiredState: "Running", OperationID: t.GetOperationId(), Revision: t.GetLifecycleRevision()}
		if err := h.acceptLifecycle(ctx, resolver, t, intent); err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}
func checkLifecycleCount(n int) error {
	if n == 0 {
		return invalid("items are required")
	}
	return checkItemCount(n)
}
func (h *Handler) acceptLifecycle(ctx context.Context, resolver *groupResolver, target *protobuf.LabLifecycleTarget, intent *lab.LabLifecycleSpec) error {
	ref := target.GetRef()
	if ref.GetLabGroup() == "" || ref.GetName() == "" || ref.GetLab() != "" {
		return fmt.Errorf("target ref requires lab_group and name, with no lab")
	}
	if target.GetExpectedLabUid() == "" || strings.TrimSpace(intent.OperationID) == "" || utf8.RuneCountInString(intent.OperationID) > 128 || intent.Revision < 1 {
		return fmt.Errorf("expected_lab_uid, operation_id (1..128 characters) and positive lifecycle_revision are required")
	}
	ns, err := resolver.namespace(ctx, ref.GetLabGroup())
	if err != nil {
		return err
	}
	group, err := resolver.get(ctx, ref.GetLabGroup())
	if err != nil {
		return err
	}
	labs := h.cs.LaboratoryV1alpha1().Labs(ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Cached resolution supplies the namespace, but each retry validates its live owner.
		live, err := h.getGroup(ctx, ref.GetLabGroup())
		if err != nil {
			return err
		}
		if err := rejectTerminating(kindLabGroup, live); err != nil {
			return err
		}
		if live.UID != group.UID || live.Status.Namespace != ns {
			return fmt.Errorf("lab group identity changed")
		}
		cur, err := labs.Get(ctx, crName(ref.GetName()), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !ownedBy(tenantOf(ctx), cur) {
			return notFoundForeign(kindLab, ref.GetName())
		}
		if err := rejectTerminating(kindLab, cur); err != nil {
			return err
		}
		if string(cur.UID) != target.GetExpectedLabUid() {
			return fmt.Errorf("lab UID differs from expected_lab_uid")
		}
		old := cur.Spec.Lifecycle
		if old != nil {
			if old.Terminal && (intent.DesiredState != "Stopped" || !intent.Terminal) {
				return fmt.Errorf("terminal lab cannot be restarted or cleared")
			}
			if intent.Revision < old.Revision {
				return fmt.Errorf("lifecycle revision is stale")
			}
			if intent.Revision == old.Revision {
				if apiequality.Semantic.DeepEqual(old, intent) {
					return nil
				}
				return fmt.Errorf("conflicting intent at equal lifecycle revision")
			}
		}
		if intent.SnapshotMode == "Required" {
			f := h.features
			r := controller.LabReconciler{Reader: snapshotAcceptanceReader{h.cs}, RequiredSnapshotAvailable: h.requiredSnapshotAvailable, State: controller.StatePolicy{Enabled: h.statePersistence, WriteQuotaBytes: f.WriteQuota, MaxFileBytes: f.MaxFileSize, TenantQuota: f.TenantQuota, MaxEntries: int32(f.MaxEntries)}}
			if err := r.ValidateRequiredSnapshot(ctx, cur); err != nil {
				return err
			}
		}
		cur.Spec.Lifecycle = intent.DeepCopy()
		_, err = labs.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

// The producer's validator consumes the same live typed API reads as acceptance.
// Only its Tenant Get and Device List dependencies are supported here.
type snapshotAcceptanceReader struct{ cs versioned.Interface }

func (r snapshotAcceptanceReader) Get(ctx context.Context, key runtimeclient.ObjectKey, obj runtimeclient.Object, _ ...runtimeclient.GetOption) error {
	out, ok := obj.(*lab.Tenant)
	if !ok {
		return fmt.Errorf("unsupported snapshot acceptance read %T", obj)
	}
	current, err := r.cs.LaboratoryV1alpha1().Tenants().Get(ctx, key.Name, metav1.GetOptions{})
	if err == nil {
		current.DeepCopyInto(out)
	}
	return err
}
func (r snapshotAcceptanceReader) List(ctx context.Context, out runtimeclient.ObjectList, opts ...runtimeclient.ListOption) error {
	devices, ok := out.(*lab.DeviceList)
	if !ok {
		return fmt.Errorf("unsupported snapshot acceptance list %T", out)
	}
	options := &runtimeclient.ListOptions{}
	options.ApplyOptions(opts)
	current, err := r.cs.LaboratoryV1alpha1().Devices(options.Namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		current.DeepCopyInto(devices)
	}
	return err
}
