package imagepull

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// CRIPuller pulls through the image service of the node's container runtime. Images land
// in the runtime's own namespace (the kubelet's), so the kubelet finds them.
type CRIPuller struct {
	conn   *grpc.ClientConn
	images criapi.ImageServiceClient
}

// NewCRIPuller connects (lazily) to the CRI socket.
func NewCRIPuller(sock string) (*CRIPuller, error) {
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial CRI %s: %w", sock, err)
	}
	return &CRIPuller{conn: conn, images: criapi.NewImageServiceClient(conn)}, nil
}

// Close releases the connection.
func (c *CRIPuller) Close() error { return c.conn.Close() }

// Present implements Puller.
func (c *CRIPuller) Present(ctx context.Context, ref string) (bool, error) {
	r, err := c.images.ImageStatus(ctx, &criapi.ImageStatusRequest{Image: &criapi.ImageSpec{Image: ref}})
	if err != nil {
		return false, err
	}
	return r.GetImage() != nil, nil
}

// Pull implements Puller.
func (c *CRIPuller) Pull(ctx context.Context, ref string, auth *Auth) error {
	req := &criapi.PullImageRequest{Image: &criapi.ImageSpec{Image: ref}}
	if auth != nil {
		req.Auth = &criapi.AuthConfig{Username: auth.Username, Password: auth.Password}
	}
	_, err := c.images.PullImage(ctx, req)
	return err
}
