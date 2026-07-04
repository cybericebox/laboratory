package grpc

import (
	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// Handler implements protobuf.LabManagerServer against the laboratory
// typed clientset (custom resources on the workload cluster).
type Handler struct {
	protobuf.UnimplementedLabManagerServer
	cs versioned.Interface
}

// NewHandler builds a Handler backed by the given typed clientset.
func NewHandler(cs versioned.Interface) *Handler {
	return &Handler{cs: cs}
}
