package grpc

import (
	"context"

	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"k8s.io/client-go/kubernetes"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Handler implements protobuf.LabManagerServer against the laboratory
// typed clientset (custom resources on the workload cluster) and, for
// resources whose data lives partly in core Secrets (e.g. LabGroupClient's
// wg.conf), the plain kubernetes clientset.
type Handler struct {
	protobuf.UnimplementedLabManagerServer
	cs  versioned.Interface
	k8s kubernetes.Interface
	// metrics is optional: nil when metrics-server is not installed. Live
	// resource-usage reporting then degrades to zero rather than failing.
	metrics metricsclient.Interface
	agentID string
}

// NewHandler builds a Handler backed by the given typed clientset, plain
// kubernetes clientset, and (optionally) a metrics clientset. A nil metrics
// client disables live usage reporting without erroring.
func NewHandler(cs versioned.Interface, k8s kubernetes.Interface, metrics metricsclient.Interface, agentID ...string) *Handler {
	id := "laboratory-agent"
	if len(agentID) > 0 && agentID[0] != "" {
		id = agentID[0]
	}
	return &Handler{cs: cs, k8s: k8s, metrics: metrics, agentID: id}
}

// Ping answers the backend's connection health probe (mTLS and CN allowlist
// have already been checked by the server interceptors).
func (h *Handler) Ping(context.Context, *protobuf.Empty) (*protobuf.Empty, error) {
	return &protobuf.Empty{}, nil
}
