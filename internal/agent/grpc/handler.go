package grpc

import (
	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"k8s.io/client-go/kubernetes"
)

// Handler implements protobuf.LabManagerServer against the laboratory
// typed clientset (custom resources on the workload cluster) and, for
// resources whose data lives partly in core Secrets (e.g. LabGroupClient's
// wg.conf), the plain kubernetes clientset.
type Handler struct {
	protobuf.UnimplementedLabManagerServer
	cs  versioned.Interface
	k8s kubernetes.Interface
}

// NewHandler builds a Handler backed by the given typed clientset and
// plain kubernetes clientset.
func NewHandler(cs versioned.Interface, k8s kubernetes.Interface) *Handler {
	return &Handler{cs: cs, k8s: k8s}
}
