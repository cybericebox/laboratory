package laboratory

import (
	"context"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Older workload/persistence fixtures deliberately omit the parent Lab so the
// suite's background Lab controller cannot rewrite their manually chosen mode.
// Supply that one Running input explicitly; Pods/Devices and all other reads
// remain real API-server operations. Lifecycle tests use real parent objects.
type standaloneRunningLabReader struct{ client.Reader }

func (r standaloneRunningLabReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.Reader.Get(ctx, key, obj, opts...)
	if target, ok := obj.(*lab.Lab); ok && apierrors.IsNotFound(err) {
		*target = lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
		return nil
	}
	return err
}

type standaloneExactLabReader struct {
	client.Reader
	parent *lab.Lab
}

func (r standaloneExactLabReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if target, ok := obj.(*lab.Lab); ok && key == client.ObjectKeyFromObject(r.parent) {
		*target = *r.parent.DeepCopy()
		return nil
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
