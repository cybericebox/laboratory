//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	nodev1 "github.com/cybericebox/laboratory/api/node/v1"
)

// NodeAgentServer implements the NodeAgent gRPC service for CNI delegation.
type NodeAgentServer struct {
	nodev1.UnimplementedNodeAgentServer
	ovs *OVSManager

	mu       sync.RWMutex
	podNetNS map[string]string // podUID → netns path
}

func newNodeAgentServer(ovs *OVSManager) *NodeAgentServer {
	return &NodeAgentServer{
		ovs:      ovs,
		podNetNS: make(map[string]string),
	}
}

// AddPort creates an OVS internal port and moves it into the pod netns.
// Called by cni-ovs during CNI ADD.
func (s *NodeAgentServer) AddPort(ctx context.Context, req *nodev1.AddPortRequest) (*nodev1.AddPortResponse, error) {
	portName := portKey(req.Namespace, req.Connection, req.InterfaceName)

	if err := s.ovs.AddInternalPort(portName); err != nil {
		return nil, fmt.Errorf("add OVS port %q: %w", portName, err)
	}

	if err := MoveToNetNS(portName, req.NetnsPath); err != nil {
		_ = s.ovs.DelPort(portName)
		return nil, fmt.Errorf("move %q to netns: %w", portName, err)
	}

	// Rename the interface inside the pod netns to the desired name.
	if req.InterfaceName != portName {
		if err := RenameInNetNS(req.NetnsPath, portName, req.InterfaceName); err != nil {
			return nil, fmt.Errorf("rename %q → %q in netns: %w", portName, req.InterfaceName, err)
		}
	}

	s.mu.Lock()
	s.podNetNS[req.PodUid] = req.NetnsPath
	s.mu.Unlock()

	return &nodev1.AddPortResponse{PortId: portName}, nil
}

// DeletePort cleans up the podNetNS cache entry.
// OVS port deletion is handled by ConnectionReconciler on Connection DELETE.
func (s *NodeAgentServer) DeletePort(_ context.Context, req *nodev1.DeletePortRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	delete(s.podNetNS, req.PodUid)
	s.mu.Unlock()
	return &emptypb.Empty{}, nil
}

// GetPodNetNS returns the cached netns path for a pod UID (used by LabIfaceReconciler).
func (s *NodeAgentServer) GetPodNetNS(podUID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ns, ok := s.podNetNS[podUID]
	return ns, ok
}

// startGRPCServer starts the NodeAgent gRPC server on a Unix socket.
func startGRPCServer(sockPath string, srv *NodeAgentServer) (*grpc.Server, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(sockPath), err)
	}
	_ = os.Remove(sockPath)

	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", sockPath, err)
	}

	s := grpc.NewServer()
	nodev1.RegisterNodeAgentServer(s, srv)

	go func() {
		_ = s.Serve(lis)
	}()
	return s, nil
}
