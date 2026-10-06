package grpc

import (
	"context"
	"sort"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

type labMatch struct {
	group string
	lab   *laboratoryv1alpha1.Lab
}

type clientMatch struct {
	group  string
	client *laboratoryv1alpha1.LabGroupClient
}

// perGroup runs list for every group that has a namespace, with bounded concurrency,
// and concatenates the answers in group-name order.
func perGroup[T any](ctx context.Context, groups []laboratoryv1alpha1.LabGroup, list func(group, namespace string) ([]T, error)) ([]T, error) {
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	parts := make([][]T, len(groups))
	errs := make([]error, len(groups))
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	for i := range groups {
		if groups[i].Status.Namespace == "" {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			parts[i], errs[i] = list(names.IDOf(&groups[i]), groups[i].Status.Namespace)
		}()
	}
	wg.Wait()
	var out []T
	for i := range parts {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out = append(out, parts[i]...)
	}
	return out, nil
}

// listLabs returns the Labs matching a label selector (empty = all) in one LabGroup
// (labGroup set) or in all of them.
func (h *Handler) listLabs(ctx context.Context, selector, labGroup string) ([]labMatch, error) {
	groups, err := h.scopedGroups(ctx, labGroup)
	if err != nil {
		return nil, err
	}
	return perGroup(ctx, groups, func(group, ns string) ([]labMatch, error) {
		list, err := h.cs.LaboratoryV1alpha1().Labs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, err
		}
		out := make([]labMatch, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, labMatch{group: group, lab: &list.Items[i]})
		}
		return out, nil
	})
}

// listClients is listLabs for LabGroupClients.
func (h *Handler) listClients(ctx context.Context, selector, labGroup string) ([]clientMatch, error) {
	groups, err := h.scopedGroups(ctx, labGroup)
	if err != nil {
		return nil, err
	}
	return perGroup(ctx, groups, func(group, ns string) ([]clientMatch, error) {
		list, err := h.cs.LaboratoryV1alpha1().LabGroupClients(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, err
		}
		out := make([]clientMatch, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, clientMatch{group: group, client: &list.Items[i]})
		}
		return out, nil
	})
}
