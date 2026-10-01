package l7

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/names"
)

// ServiceAttribution attributes a request to a lab through the web Service the
// operator creates per exposed device. The host label is the Service name; the
// Service is read (from the cache) in the namespace of the token's own group, so
// a token only reaches labs its group owns, and its lab label names the lab.
// Nothing is decoded from the host itself.
func ServiceAttribution(reader client.Reader) Attribution {
	return func(task, groupID string) (string, bool) {
		var svc corev1.Service
		key := types.NamespacedName{Name: task, Namespace: GroupNamespace(groupID)}
		if err := reader.Get(context.Background(), key, &svc); err != nil {
			return "", false
		}
		lab := svc.Labels[names.LabelLab]
		if lab == "" || svc.Labels[names.LabelDevice] == "" {
			return "", false
		}
		return lab, true
	}
}
