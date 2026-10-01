package grpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const kindDevice = "Device"

// deviceTargets resolves the targets of a device call: explicit (lab_group, lab, device)
// items, or a selector over labs plus a device name.
func (h *Handler) deviceTargets(ctx context.Context, in *protobuf.DevicesRequest) ([]*protobuf.ItemRef, error) {
	sel := in.GetBySelector()
	if err := checkSelection(sel.GetSelector(), len(in.GetItems()), sel != nil); err != nil {
		return nil, err
	}
	if sel == nil {
		for i, r := range in.GetItems() {
			if r.GetLabGroup() == "" || r.GetLab() == "" || r.GetName() == "" {
				return nil, invalid("item %d: lab_group, lab and name (the device) are required", i)
			}
		}
		if err := dupRefs(in.GetItems()); err != nil {
			return nil, err
		}
		return in.GetItems(), nil
	}
	if sel.GetDevice() == "" {
		return nil, invalid("by_selector.device is required")
	}
	matches, err := h.listLabs(ctx, sel.GetSelector(), sel.GetLabGroup())
	if err != nil {
		return nil, err
	}
	if err := checkMatched(len(matches), sel.ExpectedCount); err != nil {
		return nil, err
	}
	refs := make([]*protobuf.ItemRef, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, &protobuf.ItemRef{LabGroup: m.group, Lab: names.IDOf(m.lab), Name: sel.GetDevice()})
	}
	sortRefs(refs)
	return refs, nil
}

// patchDevices applies a Device spec patch to every target. needState: the device must
// have state persistence.
func (h *Handler) patchDevices(ctx context.Context, in *protobuf.DevicesRequest, needState bool, patch func() (map[string]any, error)) (*protobuf.BatchResult, error) {
	refs, err := h.deviceTargets(ctx, in)
	if err != nil {
		return nil, err
	}
	resolver := h.newResolver(ctx)
	return &protobuf.BatchResult{Results: forEachItem(ctx, refs, func(i int) *protobuf.ItemResult {
		spec, err := patch()
		if err != nil {
			return failedResult(refs[i], err)
		}
		if err := h.patchDevice(ctx, resolver, refs[i], needState, spec); err != nil {
			return failedResult(refs[i], err)
		}
		return result(refs[i], protobuf.ItemState_ITEM_STATE_UPDATED)
	})}, nil
}

func newToken() (string, error) {
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return hex.EncodeToString(token), nil
}

// ResetDevices discards the snapshots of the devices and restarts them from their base
// image. The operator does the work: a new reset token on the Device is what it acts
// on, so repeating the call resets again.
func (h *Handler) ResetDevices(ctx context.Context, in *protobuf.DevicesRequest) (*protobuf.BatchResult, error) {
	return h.patchDevices(ctx, in, true, func() (map[string]any, error) {
		token, err := newToken()
		return map[string]any{"state": map[string]any{"resetToken": token}}, err
	})
}

// RescueDevices switches devices between their normal start and rescue mode: the latest
// snapshot started with a shell instead of the image entrypoint.
func (h *Handler) RescueDevices(ctx context.Context, in *protobuf.RescueDevicesRequest) (*protobuf.BatchResult, error) {
	return h.patchDevices(ctx, in.GetDevices(), true, func() (map[string]any, error) {
		return map[string]any{"state": map[string]any{"rescue": in.GetEnable()}}, nil
	})
}

// patchDevice merge-patches the spec of the Device "<lab>-<device>".
func (h *Handler) patchDevice(ctx context.Context, resolver *groupResolver, ref *protobuf.ItemRef, needState bool, spec map[string]any) error {
	ns, err := resolver.namespace(ctx, ref.GetLabGroup())
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s", crName(ref.GetLab()), ref.GetName())
	devices := h.cs.LaboratoryV1alpha1().Devices(ns)
	cur, err := devices.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := rejectTerminating(kindDevice, cur); err != nil {
		return err
	}
	if needState && !cur.Spec.StateEnabled() {
		return status.Errorf(codes.FailedPrecondition, "device %s of lab %s has no state persistence", ref.GetName(), ref.GetLab())
	}
	patch, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return err
	}
	_, err = devices.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// snapshotStatusToProto maps the organizer-facing snapshot summary of a device.
func snapshotStatusToProto(s *laboratoryv1alpha1.DeviceStateInfo) *protobuf.DeviceSnapshotStatus {
	if s == nil {
		return nil
	}
	p := &protobuf.DeviceSnapshotStatus{SizeBytes: s.SizeBytes, Warning: s.QuotaWarning, Rescue: s.Rescue}
	if s.LastSnapshotAt != nil {
		p.LastSnapshotUnixMs = s.LastSnapshotAt.UnixMilli()
	}
	if s.RestoredAt != nil {
		p.RestoredUnixMs = s.RestoredAt.UnixMilli()
	}
	return p
}
