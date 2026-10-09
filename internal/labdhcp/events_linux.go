//go:build linux

package labdhcp

import (
	"context"
	"reflect"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

func RequestsForLab(ctx context.Context, c client.Client, namespace, name, network string) []reconcile.Request {
	requests := []reconcile.Request{}
	if network == networkVPN {
		var legs lab.LabVPNList
		if err := c.List(ctx, &legs, client.InNamespace(namespace)); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "list VPN DHCP legs")
			return nil
		}
		for _, leg := range legs.Items {
			if name == "" || leg.Spec.LabName == name {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&leg)})
			}
		}
	} else {
		var legs lab.LabGatewayList
		if err := c.List(ctx, &legs, client.InNamespace(namespace)); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "list gateway DHCP legs")
			return nil
		}
		for _, leg := range legs.Items {
			if name == "" || leg.Spec.LabName == name {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&leg)})
			}
		}
	}
	return requests
}
func WatchDHCP(mgr ctrl.Manager, b *builder.Builder, c client.Client, dhcpMgr *dhcp.Manager, namespace, network string) (*builder.Builder, error) {
	events := make(chan event.GenericEvent)
	err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		defer dhcpMgr.Close()
		defer close(events)
		for {
			select {
			case <-ctx.Done():
				return nil
			case change := <-dhcpMgr.Changes():
				select {
				case events <- event.GenericEvent{Object: &lab.Lab{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: change.Name}}}:
				case <-ctx.Done():
					return nil
				}
			}
		}
	}))
	if err != nil {
		return nil, err
	}
	mapping := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		name := obj.GetName()
		if p, ok := obj.(*allocation.Pool); ok {
			name = PoolLabName(p, network)
			if name == "" {
				return nil
			}
		}
		return RequestsForLab(ctx, c, obj.GetNamespace(), name, network)
	})
	labChanges := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		a, aok := e.ObjectOld.(*lab.Lab)
		z, zok := e.ObjectNew.(*lab.Lab)
		return !aok || !zok || InputsChanged(a, z, network)
	}}
	poolChanges := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		a, aok := e.ObjectOld.(*allocation.Pool)
		z, zok := e.ObjectNew.(*allocation.Pool)
		return !aok || !zok || !reflect.DeepEqual(a.Spec, z.Spec) || !reflect.DeepEqual(a.OwnerReferences, z.OwnerReferences) || !reflect.DeepEqual(a.DeletionTimestamp, z.DeletionTimestamp)
	}}
	return b.Watches(&lab.Lab{}, mapping, builder.WithPredicates(labChanges)).Watches(&allocation.Pool{}, mapping, builder.WithPredicates(poolChanges)).WatchesRawSource(source.Channel(events, mapping)), nil
}
