package laboratory

import (
	"context"
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// No pod of a device, the VPN or a gateway carries an image pull secret. The tenant travels in the image reference
// (<prefix>/c/<tenant segment>/<registry>/<repo>, imagecache.Rewriter), the registry gate identifies the node by its own token and reads
// the upstream credentials from the namespace laboratory-upstream (upstream_secrets.go). What is left here is the pull policy of the
// cache references and the name of a tenant's credentials Secret.

// tenantPullSecret is the name of the registry Secret a tenant brings, in the tenants
// namespace; "" when the tenant has none or does not exist.
func tenantPullSecret(ctx context.Context, c client.Reader, tenantName string) (string, error) {
	var t laboratoryv1alpha1.Tenant
	if err := c.Get(ctx, types.NamespacedName{Name: tenantName}, &t); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	return t.Spec.Images.PullSecret, nil
}

// errPullSecretPending: the lab names a credentials Secret that does not exist yet. The agent writes it right after the Lab, so this is
// a short wait, retried, never a failure of the lab.
var errPullSecretPending = errors.New("the lab's image pull secret does not exist yet")

// pullSecretRetry is how soon a lab that waits for its credentials Secret is looked at again.
const pullSecretRetry = 2 * time.Second

// isCacheRef says whether an image reference points at the image cache.
func isCacheRef(image, prefix string) bool {
	return prefix != "" && strings.HasPrefix(image, prefix+"/")
}

// pullPolicyFor is the image pull policy of an image the platform runs: Always for a cache reference, otherwise the node pulls it only
// when it does not have it. With the tenant in the name, an image of tenant A on the disk of a node is not the image of tenant B, and
// whether the runtime resolves it by name is not something to rely on: Always makes the kubelet ask the gate for the manifest on every
// start, which the gate authorises by tenant, schedule and upstream proof, and layers already on the node are not downloaded again.
func pullPolicyFor(image, prefix string) corev1.PullPolicy {
	if isCacheRef(image, prefix) {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

// setPullPolicies is the one place that gives the containers and init containers of a pod their pull policy (pullPolicyFor): a
// container with a cache reference pulls with Always, any other keeps the policy it has. It never adds an image pull secret.
func setPullPolicies(spec *corev1.PodSpec, prefix string) {
	for i := range spec.InitContainers {
		if isCacheRef(spec.InitContainers[i].Image, prefix) {
			spec.InitContainers[i].ImagePullPolicy = corev1.PullAlways
		}
	}
	for i := range spec.Containers {
		if isCacheRef(spec.Containers[i].Image, prefix) {
			spec.Containers[i].ImagePullPolicy = corev1.PullAlways
		}
	}
}
