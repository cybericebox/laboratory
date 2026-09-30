package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestPing(t *testing.T) {
	h := NewHandler(fake.NewSimpleClientset(), k8sfake.NewSimpleClientset(), nil)
	out, err := h.Ping(context.Background(), &protobuf.Empty{})
	if err != nil || out == nil {
		t.Fatalf("Ping: %v %v", out, err)
	}
	if status.Code(err) == codes.Unimplemented {
		t.Fatal("Ping must be implemented")
	}
}
