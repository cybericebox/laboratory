package grpc

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	// maxItems bounds the items of one call and the objects one selector may match.
	maxItems = 5000
	// maxConcurrency bounds the Kubernetes API calls one request has in flight.
	maxConcurrency = 16
)

// checkItemCount refuses a request with more items than a call may carry.
func checkItemCount(n int) error {
	if n > maxItems {
		return invalid("%d items, at most %d per call", n, maxItems)
	}
	return nil
}

// forEachItem runs fn for every ref with bounded concurrency and returns the
// results in the order of refs. After the context ends the remaining items fail.
func forEachItem(ctx context.Context, refs []*protobuf.ItemRef, fn func(i int) *protobuf.ItemResult) []*protobuf.ItemResult {
	n := len(refs)
	out := make([]*protobuf.ItemResult, n)
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			out[i] = failedResult(refs[i], ctx.Err())
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fn(i)
		}()
	}
	wg.Wait()
	return out
}

func result(ref *protobuf.ItemRef, state protobuf.ItemState) *protobuf.ItemResult {
	return &protobuf.ItemResult{Ref: ref, State: state}
}

// failedResult reports an item that did not go through. NotFound maps to
// NOT_FOUND, not FAILED.
func failedResult(ref *protobuf.ItemRef, err error) *protobuf.ItemResult {
	msg := err.Error()
	if st, ok := status.FromError(err); ok {
		msg = st.Message()
	}
	if apierrors.IsNotFound(err) {
		return &protobuf.ItemResult{Ref: ref, State: protobuf.ItemState_ITEM_STATE_NOT_FOUND, Error: msg}
	}
	return &protobuf.ItemResult{Ref: ref, State: protobuf.ItemState_ITEM_STATE_FAILED, Error: msg, Retryable: isRetryable(err)}
}

// notReadyError is a LabGroup whose namespace the operator has not created yet.
type notReadyError struct{ group string }

func (e notReadyError) Error() string {
	return fmt.Sprintf("lab group %q namespace is not ready", e.group)
}

// isRetryable tells failures a repetition can cure.
func isRetryable(err error) bool {
	if _, ok := err.(pendingAdmissionError); ok {
		return true
	}
	if client.IsTerminating(err) {
		return true
	}
	if _, ok := err.(notReadyError); ok {
		return true
	}
	return apierrors.IsConflict(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err)
}

// groupResolver resolves LabGroup names to namespaces once per call, however many
// items name the same group.
type groupResolver struct {
	h       *Handler
	mu      sync.Mutex
	entries map[string]*groupEntry
}

type groupEntry struct {
	once  sync.Once
	group *laboratoryv1alpha1.LabGroup
	err   error
}

func (h *Handler) newResolver(_ context.Context) *groupResolver {
	return &groupResolver{h: h, entries: map[string]*groupEntry{}}
}

func (r *groupResolver) get(ctx context.Context, name string) (*laboratoryv1alpha1.LabGroup, error) {
	r.mu.Lock()
	e := r.entries[name]
	if e == nil {
		e = &groupEntry{}
		r.entries[name] = e
	}
	r.mu.Unlock()
	e.once.Do(func() {
		e.group, e.err = r.h.getGroup(ctx, name)
	})
	return e.group, e.err
}

// namespace is the namespace of a live LabGroup that is ready for objects.
func (r *groupResolver) namespace(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", invalid("lab_group is required")
	}
	g, err := r.get(ctx, name)
	if err != nil {
		return "", err
	}
	if err := rejectTerminating(kindLabGroup, g); err != nil {
		return "", err
	}
	if g.Status.Namespace == "" {
		return "", notReadyError{group: name}
	}
	return g.Status.Namespace, nil
}

// scopedGroups lists the groups of the caller's tenant a selector call looks at: the named
// one, or every one.
func (h *Handler) scopedGroups(ctx context.Context, labGroup string) ([]laboratoryv1alpha1.LabGroup, error) {
	if labGroup != "" {
		g, err := h.getGroup(ctx, labGroup)
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []laboratoryv1alpha1.LabGroup{*g}, nil
	}
	return h.listGroups(ctx, "")
}

// parseSelector validates a label selector; empty is allowed only when allowEmpty.
func parseSelector(sel string, allowEmpty bool) error {
	if sel == "" {
		if allowEmpty {
			return nil
		}
		return invalid("selector must not be empty")
	}
	if err := checkSelectorKeys(sel); err != nil {
		return invalid("invalid selector: %v", err)
	}
	return nil
}

// checkSelection is the guard of Update, Delete and the device calls: exactly one of a
// non-empty selector or explicit items.
func checkSelection(sel string, nItems int, selectorSet bool) error {
	switch {
	case selectorSet && nItems > 0:
		return invalid("give a selector or items, not both")
	case selectorSet:
		return parseSelector(sel, false)
	case nItems == 0:
		return invalid("give a selector or items")
	}
	return checkItemCount(nItems)
}

// checkMatched applies the selector guards to the number of matched objects.
func checkMatched(matched int, expected *int64) error {
	if expected != nil && int64(matched) > *expected {
		return countMismatch(matched, *expected)
	}
	if matched > maxItems {
		return status.Errorf(codes.FailedPrecondition, "the selector matches %d objects, at most %d per call", matched, maxItems)
	}
	return nil
}

func countMismatch(actual int, expected int64) error {
	st := status.Newf(codes.FailedPrecondition, "the selector matches %d objects, expected at most %d; nothing was done", actual, expected)
	if withDetails, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: client.ReasonCountMismatch,
		Domain: client.ErrorDomain,
		Metadata: map[string]string{
			"actual":   strconv.Itoa(actual),
			"expected": strconv.FormatInt(expected, 10),
		},
	}); err == nil {
		st = withDetails
	}
	return st.Err()
}

// refKey orders refs for the results of selector calls.
func refKey(r *protobuf.ItemRef) string {
	return r.GetLabGroup() + "\x00" + r.GetLab() + "\x00" + r.GetName()
}

func sortRefs(refs []*protobuf.ItemRef) {
	sort.Slice(refs, func(i, j int) bool { return refKey(refs[i]) < refKey(refs[j]) })
}

// dupRefs returns an error naming the first ref that appears twice: parallel
// writes to one object would race.
func dupRefs(refs []*protobuf.ItemRef) error {
	seen := make(map[string]bool, len(refs))
	for _, r := range refs {
		k := refKey(r)
		if seen[k] {
			return invalid("%s is listed twice", describeRef(r))
		}
		seen[k] = true
	}
	return nil
}

func describeRef(r *protobuf.ItemRef) string {
	switch {
	case r.GetLab() != "":
		return r.GetLabGroup() + "/" + r.GetLab() + "/" + r.GetName()
	case r.GetLabGroup() != "":
		return r.GetLabGroup() + "/" + r.GetName()
	}
	return r.GetName()
}
