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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nodev1 "github.com/cybericebox/laboratory/api/node/v1"
)

// NodeAgentServer implements the NodeAgent gRPC service for CNI delegation.
type NodeAgentServer struct {
	nodev1.UnimplementedNodeAgentServer
	ovs *OVSManager

	k8sMu sync.RWMutex
	k8s   client.Client // set via SetK8sClient after manager is ready

	mu       sync.RWMutex
	podNetNS map[string]string   // podUID → netns path
	podPorts map[string][]string // podUID → OVS port names
}

func newNodeAgentServer(ovs *OVSManager) *NodeAgentServer {
	return &NodeAgentServer{
		ovs:      ovs,
		podNetNS: make(map[string]string),
		podPorts: make(map[string][]string),
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
	s.podPorts[req.PodUid] = append(s.podPorts[req.PodUid], portName)
	s.mu.Unlock()

	return &nodev1.AddPortResponse{PortId: portName}, nil
}

// DeletePort cleans up OVS ports and cache entries.
// Deletes all OVS ports tracked for the pod, then removes netns and port caches.
func (s *NodeAgentServer) DeletePort(_ context.Context, req *nodev1.DeletePortRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	for _, p := range s.podPorts[req.PodUid] {
		_ = s.ovs.DelPort(p)
	}
	delete(s.podNetNS, req.PodUid)
	delete(s.podPorts, req.PodUid)
	s.mu.Unlock()
	return &emptypb.Empty{}, nil
}

// SetK8sClient provides the Kubernetes API client. Called from main after the manager is created.
func (s *NodeAgentServer) SetK8sClient(c client.Client) {
	s.k8sMu.Lock()
	s.k8s = c
	s.k8sMu.Unlock()
}

// GetPodAnnotation reads a single pod annotation on behalf of the CNI plugin, which has no
// direct access to the Kubernetes API.
func (s *NodeAgentServer) GetPodAnnotation(ctx context.Context, req *nodev1.GetPodAnnotationRequest) (*nodev1.GetPodAnnotationResponse, error) {
	s.k8sMu.RLock()
	k8s := s.k8s
	s.k8sMu.RUnlock()
	if k8s == nil {
		return nil, fmt.Errorf("node-agent: not ready")
	}
	var pod corev1.Pod
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &pod); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return &nodev1.GetPodAnnotationResponse{Found: false}, nil
		}
		return nil, err
	}
	val, found := pod.Annotations[req.Key]
	return &nodev1.GetPodAnnotationResponse{Value: val, Found: found}, nil
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
