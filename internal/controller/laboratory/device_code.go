package laboratory

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// Every container device has a short random code. The workload of the device is
// named <device>-<code>, and so is its web Service, so the lab never appears in
// a pod name and two labs with a device of the same name can share a namespace.
// The code is drawn once, when the device is first needed, and then lives in
// Device.spec.code. Uniqueness in the namespace is checked against the codes of
// the other devices and the Services, with reads from the API server: the lab
// reconciler is the only one that draws codes and it runs one lab at a time.

func (r *LabReconciler) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// codeAllocator hands out the codes of one reconcile of a lab.
type codeAllocator struct {
	r    *LabReconciler
	lab  *laboratoryv1alpha1.Lab
	used map[string]bool // taken <device>-<code> names in the namespace
}

func (r *LabReconciler) newCodeAllocator(lab *laboratoryv1alpha1.Lab) *codeAllocator {
	return &codeAllocator{r: r, lab: lab}
}

func (a *codeAllocator) load(ctx context.Context) error {
	if a.used != nil {
		return nil
	}
	used := map[string]bool{}
	var devices laboratoryv1alpha1.DeviceList
	if err := a.r.reader().List(ctx, &devices, client.InNamespace(a.lab.Namespace)); err != nil {
		return err
	}
	for i := range devices.Items {
		if d := &devices.Items[i]; d.Spec.Code != "" {
			used[names.WebHostLabel(d.Spec.Name, d.Spec.Code)] = true
		}
	}
	var services corev1.ServiceList
	if err := a.r.reader().List(ctx, &services, client.InNamespace(a.lab.Namespace)); err != nil {
		return err
	}
	for i := range services.Items {
		used[services.Items[i].Name] = true
	}
	a.used = used
	return nil
}

// codeFor returns the code of a container device of the lab: the one it already
// has (on its Device, or in the name of its web Service when the Device is not
// created yet), a new one for a device that has none, and "" for a device that
// exists without a code (it predates codes and keeps its names).
func (a *codeAllocator) codeFor(ctx context.Context, device string) (string, error) {
	var existing laboratoryv1alpha1.Device
	err := a.r.reader().Get(ctx, types.NamespacedName{Namespace: a.lab.Namespace, Name: a.lab.Name + "-" + device}, &existing)
	switch {
	case err == nil:
		return existing.Spec.Code, nil
	case !errors.IsNotFound(err):
		return "", err
	}
	svc, err := a.r.findWebService(ctx, a.lab, device)
	if err != nil {
		return "", err
	}
	if svc != nil {
		if code, ok := strings.CutPrefix(svc.Name, device+"-"); ok && code != "" {
			return code, nil
		}
	}
	if err := a.load(ctx); err != nil {
		return "", err
	}
	draw := a.r.newWebCode
	if draw == nil {
		draw = names.NewWebCode
	}
	for attempt := 0; attempt < 2*names.WebCodeAttempts; attempt++ {
		n := names.WebCodeLen
		if attempt >= names.WebCodeAttempts {
			n = names.WebCodeMaxLen
		}
		code, err := draw(n)
		if err != nil {
			return "", err
		}
		if name := names.WebHostLabel(device, code); !a.used[name] {
			a.used[name] = true
			return code, nil
		}
	}
	return "", fmt.Errorf("no free code for device %s", device)
}
