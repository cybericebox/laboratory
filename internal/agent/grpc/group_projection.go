package grpc

import (
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func groupIntentToProto(s *lab.GroupLifecycleSpec) *protobuf.GroupLifecycleSpec {
	if s == nil {
		return nil
	}
	return &protobuf.GroupLifecycleSpec{DesiredState: s.DesiredState, OperationId: s.OperationID, Revision: s.Revision, RequireAllLabsStopped: s.RequireAllLabsStopped}
}
func groupLifecycleToProto(g *lab.LabGroup) *protobuf.LabLifecycleStatus {
	s := g.Spec.Lifecycle
	if s == nil {
		return nil
	}
	out := &protobuf.LabLifecycleStatus{DesiredState: s.DesiredState, ObservedState: "Unknown", OperationId: s.OperationID, LifecycleRevision: s.Revision, LabUid: string(g.UID)}
	o := g.Status.Lifecycle
	if o == nil || o.LabUID != string(g.UID) || o.OperationID != s.OperationID || o.Revision != s.Revision || o.ObservedGeneration != g.Generation {
		return out
	}
	out.ObservedState = o.ObservedState
	out.ObservedGeneration = o.ObservedGeneration
	out.Reason = o.Reason
	out.Error = o.Error
	out.RequestedUnixMs = ms(o.RequestedAt)
	out.StoppedUnixMs = ms(o.StoppedAt)
	return out
}
