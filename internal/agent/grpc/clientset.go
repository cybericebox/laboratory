package grpc

import (
	"k8s.io/client-go/rest"

	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
)

// NewVersionedClientset builds the laboratory typed clientset forcing JSON
// content negotiation. The clientset is generated with --prefers-protobuf,
// but CRDs are served only as JSON, so protobuf negotiation fails at runtime.
func NewVersionedClientset(cfg *rest.Config) (versioned.Interface, error) {
	c := rest.CopyConfig(cfg)
	c.ContentType = "application/json"
	return versioned.NewForConfig(c)
}
