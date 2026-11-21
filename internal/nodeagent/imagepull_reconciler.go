package nodeagent

import (
	"context"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagepull"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

// ImagePullReconciler answers the scheduler's image prepull requests (ImagePull) by pulling
// the images onto this node through the container runtime. Nothing from an image runs. The references of the request already carry the
// tenant (<prefix>/c/<tenant>/<registry>/<repo>): the node-agent reads no Secret and sends no credential, the registry gate decides by the
// node and the reference.
// Each node-agent writes only its own entry of the status.
type ImagePullReconciler struct {
	client.Client
	NodeName string
	Puller   imagepull.Puller
	// Concurrency is how many images are pulled at once; Timeout bounds one image.
	Concurrency int
	Timeout     time.Duration
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=imagepulls,verbs=get;list;watch
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=imagepulls/status,verbs=get;update;patch

func (r *ImagePullReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	var ip laboratoryv1alpha1.ImagePull
	if err := r.Get(ctx, req.NamespacedName, &ip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ip.DeletionTimestamp.IsZero() || !slices.Contains(ip.Spec.Nodes, r.NodeName) {
		return ctrl.Result{}, nil
	}
	if ip.Status.Nodes[r.NodeName].Done {
		return ctrl.Result{}, nil
	}

	log.Info("pulling images", "request", ip.Name, "images", len(ip.Spec.Images))
	res := imagepull.Pull(ctx, r.Puller, ip.Spec.Images, imagepull.Options{Concurrency: r.Concurrency, Timeout: r.Timeout})
	if ctx.Err() != nil {
		return ctrl.Result{}, ctx.Err()
	}

	now := metav1.Now()
	entry := laboratoryv1alpha1.NodeImagePull{Pulled: res.Pulled, Done: true, UpdatedAt: &now}
	for _, f := range res.Failed {
		entry.Failed = append(entry.Failed, laboratoryv1alpha1.ImagePullFailure{Image: f.Image, Message: f.Message})
	}
	if len(entry.Failed) > 0 {
		log.Info("some images could not be pulled", "request", ip.Name, "failed", len(entry.Failed))
	}
	orig := ip.DeepCopy()
	if ip.Status.Nodes == nil {
		ip.Status.Nodes = map[string]laboratoryv1alpha1.NodeImagePull{}
	}
	ip.Status.Nodes[r.NodeName] = entry
	return ctrl.Result{}, client.IgnoreNotFound(r.Status().Patch(ctx, &ip, client.MergeFrom(orig)))
}

func (r *ImagePullReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.ImagePull{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(reconcileutil.QuietIgnoreNotFound(r))
}
