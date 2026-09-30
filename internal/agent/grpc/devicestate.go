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
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const kindDevice = "Device"

// ResetDevice discards the snapshots of a device and restarts it from its base
// image. The operator does the work: a new reset token on the Device is what it
// acts on, so repeating the call resets again.
func (h *Handler) ResetDevice(ctx context.Context, in *protobuf.DeviceRequest) (*protobuf.Empty, error) {
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	if err := h.patchDeviceState(ctx, in.Namespace, in.Lab, in.Device, map[string]any{"resetToken": hex.EncodeToString(token)}); err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}

// RescueDevice switches a device between its normal start and rescue mode: the
// latest snapshot started with a shell instead of the image entrypoint.
func (h *Handler) RescueDevice(ctx context.Context, in *protobuf.RescueDeviceRequest) (*protobuf.Empty, error) {
	if err := h.patchDeviceState(ctx, in.Namespace, in.Lab, in.Device, map[string]any{"rescue": in.Enable}); err != nil {
		return nil, err
	}
	return &protobuf.Empty{}, nil
}

// patchDeviceState merges fields into spec.state of a device that has state persistence.
func (h *Handler) patchDeviceState(ctx context.Context, namespace, lab, device string, fields map[string]any) error {
	if namespace == "" || lab == "" || device == "" {
		return status.Error(codes.InvalidArgument, "namespace, lab and device are required")
	}
	name := fmt.Sprintf("%s-%s", lab, device)
	devices := h.cs.LaboratoryV1alpha1().Devices(namespace)
	cur, err := devices.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := rejectTerminating(kindDevice, cur); err != nil {
		return err
	}
	if !cur.Spec.StateEnabled() {
		return status.Errorf(codes.FailedPrecondition, "device %s of lab %s has no state persistence", device, lab)
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"state": fields}})
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
