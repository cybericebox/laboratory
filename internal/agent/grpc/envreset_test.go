package grpc

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// resetCluster empties the shared API server after a test, so that the next one finds the cluster as the first did.
//
// envtest has no controller manager: nothing garbage-collects, nothing finishes the deletion of a namespace and no pod ever stops
// terminating. So every object of the kinds the tests create is deleted by hand (finalizers removed first, grace period 0), and
// every namespace but the system ones is finalized through its /finalize subresource, which is what the namespace controller does.
// The CRDs, the system namespaces and the objects the API server makes for itself stay.

// resettableGroups are the API groups whose objects are cleared. Anything else (the API server's own flow control, bootstrap RBAC,
// events, the CRDs) is left alone.
var resettableGroups = map[string]bool{
	"":                           true,
	"laboratory.cybericebox.com": true,
	"allocation.cybericebox.com": true,
	"apps":                       true,
	"batch":                      true,
	"coordination.k8s.io":        true,
	"discovery.k8s.io":           true,
	"networking.k8s.io":          true,
	"policy":                     true,
	"rbac.authorization.k8s.io":  true,
}

var systemNamespaces = map[string]bool{"default": true, "kube-system": true, "kube-public": true, "kube-node-lease": true}

// skipped resources of the resettable groups.
var skipResources = map[string]bool{"events": true, "bindings": true, "componentstatuses": true, "namespaces": true, "nodes/proxy": true}

var resetPlan struct {
	once sync.Once
	gvrs []resetTarget
	err  error
}

type resetTarget struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}

func planReset(cfg *rest.Config) ([]resetTarget, error) {
	resetPlan.once.Do(func() {
		disc, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			resetPlan.err = err
			return
		}
		lists, err := disc.ServerPreferredResources()
		if len(lists) == 0 && err != nil {
			resetPlan.err = err
			return
		}
		for _, l := range lists {
			gv, perr := schema.ParseGroupVersion(l.GroupVersion)
			if perr != nil || !resettableGroups[gv.Group] {
				continue
			}
			for _, r := range l.APIResources {
				if strings.Contains(r.Name, "/") || skipResources[r.Name] || !hasVerbs(r.Verbs, "list", "delete") {
					continue
				}
				resetPlan.gvrs = append(resetPlan.gvrs, resetTarget{gvr: gv.WithResource(r.Name), namespaced: r.Namespaced})
			}
		}
	})
	return resetPlan.gvrs, resetPlan.err
}

func hasVerbs(have metav1.Verbs, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}

// ownedByTheAPIServer says the object is one the API server (or its bootstrap) made and must keep.
func ownedByTheAPIServer(u *unstructured.Unstructured) bool {
	name := u.GetName()
	if strings.HasPrefix(name, "system:") || strings.HasPrefix(name, "system-") || u.GetLabels()["kubernetes.io/bootstrapping"] != "" {
		return true
	}
	if systemNamespaces[u.GetNamespace()] && (name == "kubernetes" || name == "default" || name == "kube-root-ca.crt") {
		return true
	}
	return false
}

func resetCluster(t *testing.T, cfg *rest.Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	targets, err := planReset(cfg)
	if err != nil {
		t.Errorf("reset the cluster: %v", err)
		return
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Errorf("reset the cluster: %v", err)
		return
	}
	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Errorf("reset the cluster: %v", err)
		return
	}
	zero := int64(0)
	for _, target := range targets {
		list, err := dyn.Resource(target.gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue // a kind this API server does not serve right now
		}
		for i := range list.Items {
			u := &list.Items[i]
			if ownedByTheAPIServer(u) {
				continue
			}
			var res dynamic.ResourceInterface = dyn.Resource(target.gvr)
			if target.namespaced {
				res = dyn.Resource(target.gvr).Namespace(u.GetNamespace())
			}
			if len(u.GetFinalizers()) > 0 {
				_, err = res.Patch(ctx, u.GetName(), types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					t.Errorf("reset: remove the finalizers of %s %s/%s: %v", target.gvr.Resource, u.GetNamespace(), u.GetName(), err)
				}
			}
			if err = res.Delete(ctx, u.GetName(), metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("reset: delete %s %s/%s: %v", target.gvr.Resource, u.GetNamespace(), u.GetName(), err)
			}
		}
	}
	namespaces, err := k8s.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Errorf("reset: list the namespaces: %v", err)
		return
	}
	for i := range namespaces.Items {
		ns := &namespaces.Items[i]
		if systemNamespaces[ns.Name] {
			continue
		}
		if ns.DeletionTimestamp.IsZero() {
			if err := k8s.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("reset: delete the namespace %s: %v", ns.Name, err)
				continue
			}
		}
		// The namespace controller would do this; without it the namespace stays Terminating and nothing can be created in it again.
		fresh, err := k8s.CoreV1().Namespaces().Get(ctx, ns.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			t.Errorf("reset: read the namespace %s: %v", ns.Name, err)
			continue
		}
		fresh.Spec.Finalizers = nil
		if _, err := k8s.CoreV1().Namespaces().Finalize(ctx, fresh, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("reset: finalize the namespace %s: %v", ns.Name, err)
		}
	}
}
