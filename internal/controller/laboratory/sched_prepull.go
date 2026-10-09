package laboratory

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// Labels of the ImagePull request that pulls the images of a launch class.
const (
	prepullLabel      = "laboratory.cybericebox.com/prepull"
	prepullClassLabel = "laboratory.cybericebox.com/prepull-class"
	managedByLabel    = "app.kubernetes.io/managed-by"
	managedByValue    = "laboratory-operator"

	// maxPrepullImages bounds the images of one request. A class has a handful of
	// images; a larger set is cut, and the rest is pulled on demand.
	maxPrepullImages = 40
)

// prepClass is the key of a launch class: the tenant is part of it, so two tenants that
// use the same deploy group name never share a prepull (their credentials differ).
func prepClass(tenant, class string) string { return "t/" + tenant + "/" + class }

// prepullKey is the stable short name suffix of a class: class ids are free text
// and not valid label values.
func prepullKey(class string) string {
	sum := sha256.Sum256([]byte(class))
	return hex.EncodeToString(sum[:])[:10]
}

func prepullName(class string) string { return "prepull-" + prepullKey(class) }

// buildImagePull builds the request that puts every image of a class on every listed
// node. The node-agents pull through their container runtime, which only fetches and
// unpacks the image: nothing from the image runs, so the lab service itself never
// starts. The kubelet then finds the image on the node and the pod starts at once.
func buildImagePull(class, tenant string, images, nodes []string) *laboratoryv1alpha1.ImagePull {
	if len(images) > maxPrepullImages {
		images = images[:maxPrepullImages]
	}
	return &laboratoryv1alpha1.ImagePull{
		ObjectMeta: metav1.ObjectMeta{
			Name:        prepullName(class),
			Labels:      map[string]string{managedByLabel: managedByValue, prepullLabel: prepullKey(class)},
			Annotations: map[string]string{prepullClassLabel: class},
		},
		Spec: laboratoryv1alpha1.ImagePullSpec{
			Images: append([]string(nil), images...),
			Nodes:  append([]string{}, nodes...),
			Tenant: tenant,
		},
	}
}

// prepullState is how far a prepull is.
type prepullState struct {
	// pulled counts the nodes that hold all their images, resolved the nodes that hold or
	// gave up on each of them, and desired the nodes asked.
	pulled, resolved, desired int
	// failed lists the images that could not be pulled, once each.
	failed []string
	// done is true once every asked node has resolved all its images; a request with no
	// node (desired 0) is done at once.
	done bool
}

// imagePullProgress reads the progress of a prepull from the status the node-agents write.
// An image that fails to pull counts as resolved at once (a pull error is reported within
// seconds), so one broken image does not hold the group until the timeout.
func imagePullProgress(ip *laboratoryv1alpha1.ImagePull) prepullState {
	st := prepullState{desired: len(ip.Spec.Nodes)}
	seen := map[string]bool{}
	for _, node := range ip.Spec.Nodes {
		n, ok := ip.Status.Nodes[node]
		if !ok || !n.Done {
			continue
		}
		st.resolved++
		if len(n.Failed) == 0 {
			st.pulled++
		}
		for _, f := range n.Failed {
			if !seen[f.Image] {
				seen[f.Image] = true
				st.failed = append(st.failed, f.Image)
			}
		}
	}
	sort.Strings(st.failed)
	st.done = st.resolved >= st.desired
	return st
}
