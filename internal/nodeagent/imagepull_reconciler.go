package nodeagent

import (
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagepull"
	"github.com/cybericebox/laboratory/internal/reconcileutil"
)

// ImagePullReconciler answers the scheduler's image prepull requests (ImagePull) by pulling
// the images onto this node through the container runtime. Nothing from an image runs.
// Each node-agent writes only its own entry of the status.
type ImagePullReconciler struct {
	client.Client
	// Reader reads Secrets straight from the API server: the node-agent has no watch on them.
	Reader          client.Reader
	NodeName        string
	ImagesNamespace string
	Puller          imagepull.Puller
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

	cred, err := r.credentials(ctx, &ip)
	if err != nil {
		return ctrl.Result{}, err
	}
	log.Info("pulling images", "request", ip.Name, "images", len(ip.Spec.Images))
	res := imagepull.Pull(ctx, r.Puller, ip.Spec.Images, imagepull.Options{
		Concurrency: r.Concurrency, Timeout: r.Timeout, Credentials: cred,
	})
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

// credentials reads the request's registry credentials. A missing Secret means an
// anonymous pull: the operator writes it before it creates the request, so it is gone
// only when the request is already being removed.
func (r *ImagePullReconciler) credentials(ctx context.Context, ip *laboratoryv1alpha1.ImagePull) (func(string) *imagepull.Auth, error) {
	if ip.Spec.PullSecret == "" {
		return func(string) *imagepull.Auth { return nil }, nil
	}
	var s corev1.Secret
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: r.ImagesNamespace, Name: ip.Spec.PullSecret}, &s); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return func(string) *imagepull.Auth { return nil }, nil
		}
		return nil, err
	}
	cred, err := imagepull.DockerConfigCredentials(s.Data[corev1.DockerConfigJsonKey])
	if err != nil {
		// A malformed document is the operator's doing; pull anonymously, the pods that need it fail on their own.
		ctrl.LoggerFrom(ctx).Error(err, "the request's pull secret is not a registry document; pulling anonymously", "request", ip.Name)
		return func(string) *imagepull.Auth { return nil }, nil
	}
	return cred, nil
}

func (r *ImagePullReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&laboratoryv1alpha1.ImagePull{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(reconcileutil.QuietIgnoreNotFound(r))
}
