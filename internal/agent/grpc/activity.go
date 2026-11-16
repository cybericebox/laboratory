package grpc

import (
	"container/heap"
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/activity"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// The page of GetLabGroupActivity.
const (
	defaultActivityLimit = 1000
	maxActivityLimit     = 5000
	// A report nobody has written to for this long (a proxy replica that is gone) is deleted when it holds no event.
	staleActivityReport = 2 * time.Hour
	activityCursorTag   = "a1."
)

// ActivityDisabledMessage is the message of the FailedPrecondition the call answers while the cluster runs without activity reports.
const ActivityDisabledMessage = "activity reports disabled"

func (h *Handler) activityFeature() *protobuf.ActivityFeature {
	return &protobuf.ActivityFeature{Enabled: h.features.ActivityReports, MaxEvents: activity.MaxEvents, RingMinutes: int32(activity.MaxAge / time.Minute)}
}

// GetLabGroupActivity hands the platform the raw activity facts of a group that came after its cursor. The laboratory keeps the facts only in
// the short rings of the writers and aggregates nothing; the platform calls about once a minute and aggregates itself.
func (h *Handler) GetLabGroupActivity(ctx context.Context, in *protobuf.GetLabGroupActivityRequest) (*protobuf.GetLabGroupActivityResponse, error) {
	if !h.features.ActivityReports {
		return nil, status.Error(codes.FailedPrecondition, ActivityDisabledMessage)
	}
	cursor, err := decodeActivityCursor(in.GetCursor())
	if err != nil {
		return nil, invalid("%v", err)
	}
	limit := int(in.GetLimit())
	switch {
	case limit < 0:
		return nil, invalid("limit must not be negative")
	case limit == 0:
		limit = defaultActivityLimit
	case limit > maxActivityLimit:
		limit = maxActivityLimit
	}
	kinds := map[laboratoryv1alpha1.ActivityKind]bool{}
	for _, k := range in.GetKinds() {
		kind, ok := activityKindOf(k)
		if !ok {
			return nil, invalid("unknown activity kind %v", k)
		}
		kinds[kind] = true
	}

	g, err := h.getGroup(ctx, in.GetLabGroupName())
	if apierrors.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "lab group %q not found", in.GetLabGroupName())
	}
	if err != nil {
		return nil, err
	}
	ns := g.Status.Namespace
	if ns == "" {
		return &protobuf.GetLabGroupActivityResponse{NextCursor: encodeActivityCursor(cursor)}, nil
	}
	list, err := h.cs.LaboratoryV1alpha1().LabActivityReports(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := mergeActivity(list.Items, cursor, kinds, limit)
	h.reapActivityReports(ctx, ns, list.Items)
	return out, nil
}

func activityKindOf(k protobuf.ActivityKind) (laboratoryv1alpha1.ActivityKind, bool) {
	switch k {
	case protobuf.ActivityKind_ACTIVITY_KIND_INTERNET_EGRESS:
		return laboratoryv1alpha1.ActivityEgress, true
	case protobuf.ActivityKind_ACTIVITY_KIND_CONSOLE_SESSION:
		return laboratoryv1alpha1.ActivityConsole, true
	case protobuf.ActivityKind_ACTIVITY_KIND_DEVICE_TOUCH:
		return laboratoryv1alpha1.ActivityTouch, true
	}
	return "", false
}

// The cursor is the last Seq the platform has taken from each writer, opaque to the caller.
func encodeActivityCursor(c map[string]int64) string {
	raw, _ := json.Marshal(c)
	return activityCursorTag + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeActivityCursor(s string) (map[string]int64, error) {
	out := map[string]int64{}
	if s == "" {
		return out, nil
	}
	if !strings.HasPrefix(s, activityCursorTag) {
		return nil, errBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, activityCursorTag))
	if err != nil || json.Unmarshal(raw, &out) != nil || len(out) > 1000 {
		return nil, errBadCursor
	}
	return out, nil
}

type cursorError string

func (e cursorError) Error() string { return string(e) }

const errBadCursor = cursorError("the cursor is not one this agent gave")

// stream is the unread events of one writer, in Seq order.
type activityStream struct {
	writer string
	events []laboratoryv1alpha1.ActivityEvent
}

type streamHeap []*activityStream

func (h streamHeap) Len() int { return len(h) }
func (h streamHeap) Less(i, j int) bool {
	a, b := h[i].events[0], h[j].events[0]
	if a.AtMs != b.AtMs {
		return a.AtMs < b.AtMs
	}
	return h[i].writer < h[j].writer
}
func (h streamHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *streamHeap) Push(x any)   { *h = append(*h, x.(*activityStream)) }
func (h *streamHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// mergeActivity takes the events after the cursor from every writer's ring, in time order with each writer's own order kept, at most limit of
// them. A writer's cursor moves over an event the kinds filter leaves out, so a filtered call does not see it again.
func mergeActivity(reports []laboratoryv1alpha1.LabActivityReport, cursor map[string]int64, kinds map[laboratoryv1alpha1.ActivityKind]bool, limit int) *protobuf.GetLabGroupActivityResponse {
	next := map[string]int64{}
	for w, s := range cursor {
		next[w] = s
	}
	resp := &protobuf.GetLabGroupActivityResponse{}
	var heads streamHeap
	for i := range reports {
		r := &reports[i]
		writer := r.Name
		after := cursor[writer]
		// A cursor that exists but is below what the writer dropped has missed events. With no cursor at all the platform asks for the start.
		if len(cursor) > 0 && r.Status.DroppedThrough > after {
			resp.Gap = true
		}
		var pending []laboratoryv1alpha1.ActivityEvent
		for _, e := range r.Status.Events {
			if e.Seq > after {
				pending = append(pending, e)
			}
		}
		sort.Slice(pending, func(a, b int) bool { return pending[a].Seq < pending[b].Seq })
		if len(pending) > 0 {
			heads = append(heads, &activityStream{writer: writer, events: pending})
		}
	}
	heap.Init(&heads)
	for heads.Len() > 0 && len(resp.Events) < limit {
		s := heads[0]
		e := s.events[0]
		s.events = s.events[1:]
		next[s.writer] = e.Seq
		if len(kinds) == 0 || kinds[e.Kind] {
			if pe := activityEventProto(e); pe != nil {
				resp.Events = append(resp.Events, pe)
			}
		}
		if len(s.events) == 0 {
			heap.Pop(&heads)
		} else {
			heap.Fix(&heads, 0)
		}
	}
	resp.HasMore = heads.Len() > 0
	resp.NextCursor = encodeActivityCursor(next)
	return resp
}

func activityEventProto(e laboratoryv1alpha1.ActivityEvent) *protobuf.ActivityEvent {
	out := &protobuf.ActivityEvent{AtMs: e.AtMs, LabName: e.LabName}
	switch e.Kind {
	case laboratoryv1alpha1.ActivityEgress:
		out.Fact = &protobuf.ActivityEvent_Egress{Egress: &protobuf.InternetEgress{
			SourceAddress: e.SourceAddr, DstAddress: e.DstAddr, DstPort: e.DstPort, Protocol: e.Protocol, BytesUp: e.BytesUp, BytesDown: e.BytesDown,
			FirstSeenMs: e.FirstSeenMs, LastSeenMs: e.LastSeenMs, Connections: e.Connections}}
	case laboratoryv1alpha1.ActivityConsole:
		out.Fact = &protobuf.ActivityEvent_Console{Console: &protobuf.ConsoleSession{
			SessionId: e.SessionID, Device: e.Device, Client: e.Client, OpenedAtMs: e.OpenedAtMs, ClosedAtMs: e.ClosedAtMs}}
	case laboratoryv1alpha1.ActivityTouch:
		out.Fact = &protobuf.ActivityEvent_Touch{Touch: &protobuf.DeviceTouch{
			Client: e.Client, DstAddress: e.DstAddr, Protocol: e.Protocol, BytesOut: e.BytesUp, BytesIn: e.BytesDown,
			FirstSeenMs: e.FirstSeenMs, LastSeenMs: e.LastSeenMs}}
	default:
		return nil
	}
	return out
}

// reapActivityReports deletes the reports of writers that are gone: empty, and not written for a long time. Best effort.
func (h *Handler) reapActivityReports(ctx context.Context, namespace string, reports []laboratoryv1alpha1.LabActivityReport) {
	cut := time.Now().Add(-staleActivityReport).UnixMilli()
	for i := range reports {
		r := &reports[i]
		if len(r.Status.Events) == 0 && r.Status.UpdatedMs != 0 && r.Status.UpdatedMs < cut {
			_ = h.cs.LaboratoryV1alpha1().LabActivityReports(namespace).Delete(ctx, r.Name, metav1.DeleteOptions{})
		}
	}
}
