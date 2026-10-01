package laboratory

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// The Lab is only an owner (not the controller) of its Devices; a change of a Device must
// still reach the Lab, or its status never follows the devices once it is Ready.
func TestDeviceChangeQueuesTheOwnerLab(t *testing.T) {
	s := pruneScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(laboratoryv1alpha1.SchemeGroupVersion.WithKind("Lab"), meta.RESTScopeNamespace)
	mapper.Add(laboratoryv1alpha1.SchemeGroupVersion.WithKind("Device"), meta.RESTScopeNamespace)
	h := labOwnerHandler(s, mapper)

	lab := codeLab("l1", "uid-1")
	dev := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{
		Name: "l1-web", Namespace: "team-alpha",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: laboratoryv1alpha1.SchemeGroupVersion.String(), Kind: "Lab", Name: lab.Name, UID: lab.UID,
		}},
	}}
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: dev, ObjectNew: dev}, q)
	if q.Len() != 1 {
		t.Fatalf("want the owner Lab queued once, got %d", q.Len())
	}
	req, _ := q.Get()
	if req.Name != "l1" || req.Namespace != "team-alpha" {
		t.Fatalf("queued %v", req)
	}
}
