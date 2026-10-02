//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/names"
	nodev1 "github.com/cybericebox/laboratory/pkg/rpc/node/v1"
)

var setupLog = ctrl.Log.WithName("setup-networks")

// NodeAgentServer implements the NodeAgent gRPC service for CNI delegation.
type NodeAgentServer struct {
	nodev1.UnimplementedNodeAgentServer
	ovs   *OVSManager
	flows *FlowManager

	k8sMu sync.RWMutex
	k8s   client.Client // set via SetK8sClient after manager is ready

	mu       sync.RWMutex
	podPorts map[string][]string // podUID → OVS port names
}

func NewNodeAgentServer(ovs *OVSManager, flows *FlowManager) *NodeAgentServer {
	return &NodeAgentServer{
		ovs:      ovs,
		flows:    flows,
		podPorts: make(map[string][]string),
	}
}

// delVethWithFlows removes the t0 entry for a veth port before deleting the
// port. OVS keeps flows referencing deleted ports, and the recycled ofport
// number would make the stale flow match a different interface.
func (s *NodeAgentServer) delVethWithFlows(stableKey string) {
	if s.flows != nil {
		_ = s.flows.DelT0Port(stableKey)
	}
	_ = s.ovs.DelVethPort(stableKey)
}

// SetupNetworks is called by cni-gate during CNI ADD. It waits for the pod to
// appear in the controller-runtime cache, creates OVS veth pairs for all
// interfaces listed in AnnotationNetworks, moves each pod-side veth into the
// provided netns, and returns how cni-gate should configure the default interface.
func (s *NodeAgentServer) SetupNetworks(
	ctx context.Context,
	req *nodev1.SetupNetworksRequest,
) (*nodev1.SetupNetworksResponse, error) {
	s.k8sMu.RLock()
	k8s := s.k8s
	s.k8sMu.RUnlock()
	if k8s == nil {
		return nil, fmt.Errorf("node-agent: not ready")
	}

	// Wait for pod to appear in the controller-runtime cache.
	deadline := time.Now().Add(10 * time.Second)
	var pod corev1.Pod
	for {
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &pod); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			if time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			return nil, fmt.Errorf("pod %s/%s not found in cache after 10s", req.Namespace, req.Name)
		}
		break
	}

	defaultIface, hasAnnotation := pod.Annotations[names.AnnotationDefaultNetwork]
	log := setupLog.WithValues(
		"pod", req.Namespace+"/"+req.Name,
		"defaultNetwork", defaultIface, "hasAnnotation", hasAnnotation,
	)

	// The VPN pod asks for conntrack byte accounting in its namespace (it cannot switch it on unprivileged).
	if pod.Annotations[names.AnnotationConntrackAccounting] == "true" {
		if err := EnableConntrackAccounting(req.NetnsPath); err != nil {
			log.Error(err, "conntrack accounting unavailable: flow bytes stay zero")
		}
	}

	// Regular pod or explicit eth0: tell cni-gate to delegate normally.
	if !hasAnnotation || defaultIface == names.DefaultEth0 {
		log.Info("default network: REAL eth0 (delegate to k8s CNI)")
		return &nodev1.SetupNetworksResponse{DefaultNetwork: "real"}, nil
	}

	// Device pod or access-port pod: wire all OVS interfaces synchronously.
	attachments := ParseNetworkAnnotation(pod.Annotations[names.AnnotationNetworks])
	log.Info("wiring OVS interfaces synchronously", "count", len(attachments))
	for _, att := range attachments {
		stableKey := names.DevicePortKey(req.Namespace, req.Name, att.Iface)
		podSide := VethPeerName(stableKey)
		targetIface := att.Iface

		if _, exists, err := s.ovs.FindPortByKey(stableKey); err != nil {
			return nil, fmt.Errorf("find veth port %q: %w", stableKey, err)
		} else if !exists {
			if err := s.ovs.AddVethPort(stableKey); err != nil {
				return nil, fmt.Errorf("add veth port %q: %w", stableKey, err)
			}
		}

		podSideInRoot := WaitForLink(podSide, 200*time.Millisecond) == nil
		if podSideInRoot {
			if CheckInNetNS(req.NetnsPath, targetIface) == nil {
				_ = DeleteInNetNS(req.NetnsPath, targetIface)
			}
			if err := MoveToNetNS(podSide, req.NetnsPath); err != nil {
				return nil, fmt.Errorf("move %q to netns: %w", podSide, err)
			}
			if podSide != targetIface {
				if err := RenameInNetNS(req.NetnsPath, podSide, targetIface); err != nil {
					return nil, fmt.Errorf("rename %q → %q: %w", podSide, targetIface, err)
				}
			}
		} else if CheckInNetNS(req.NetnsPath, targetIface) == nil {
			// Already in pod netns under correct name — idempotent, fall through to BringUp.
		} else if CheckInNetNS(req.NetnsPath, podSide) == nil {
			// Moved but not yet renamed.
			if podSide != targetIface {
				if err := RenameInNetNS(req.NetnsPath, podSide, targetIface); err != nil {
					return nil, fmt.Errorf("rename (recovery) %q → %q: %w", podSide, targetIface, err)
				}
			}
		} else {
			// Not in root netns and not in pod netns — veth lost, recreate.
			s.delVethWithFlows(stableKey)
			return nil, fmt.Errorf("veth %q lost (not in root or pod netns); recreate triggered", stableKey)
		}
		if att.MAC != "" {
			if err := SetMACInNetNS(req.NetnsPath, targetIface, att.MAC); err != nil {
				return nil, fmt.Errorf("set MAC on %q: %w", targetIface, err)
			}
		}
		if err := BringUpInNetNS(req.NetnsPath, targetIface); err != nil {
			return nil, fmt.Errorf("bring up %q: %w", targetIface, err)
		}
		// node-agent is L2 only: veth moved in, renamed, MAC set, link up.
		// IP/route configuration is the device init-container's job.
	}

	if defaultIface == "" {
		log.Info("default network: NONE (stub eth0 only, no default-network connection)")
		return &nodev1.SetupNetworksResponse{DefaultNetwork: "stub"}, nil
	}
	log.Info("default network: ACCESS port (delegate k8s CNI to named iface)", "iface", defaultIface)
	// Access port: cni-gate delegates k8s CNI to this interface name + adds stub eth0.
	return &nodev1.SetupNetworksResponse{DefaultNetwork: defaultIface}, nil
}

// AddPort creates a veth pair and moves the pod-side into the pod netns.
// Reserved for external CNI-style callers; current lab-port wiring runs
// inline in ConnectionReconciler.reconcileCreate.
func (s *NodeAgentServer) AddPort(ctx context.Context, req *nodev1.AddPortRequest) (*nodev1.AddPortResponse, error) {
	stableKey := portKey(req.Namespace, req.Connection, req.InterfaceName)
	podSide := VethPeerName(stableKey)

	if err := s.ovs.AddVethPort(stableKey); err != nil {
		return nil, fmt.Errorf("add veth port %q: %w", stableKey, err)
	}

	if err := MoveToNetNS(podSide, req.NetnsPath); err != nil {
		_ = s.ovs.DelVethPort(stableKey)
		return nil, fmt.Errorf("move %q to netns: %w", podSide, err)
	}

	// Rename the pod-side inside the pod netns to the desired name.
	if req.InterfaceName != podSide {
		if err := RenameInNetNS(req.NetnsPath, podSide, req.InterfaceName); err != nil {
			return nil, fmt.Errorf("rename %q → %q in netns: %w", podSide, req.InterfaceName, err)
		}
	}
	if err := BringUpInNetNS(req.NetnsPath, req.InterfaceName); err != nil {
		return nil, fmt.Errorf("bring up %q in netns: %w", req.InterfaceName, err)
	}

	s.mu.Lock()
	s.podPorts[req.PodUid] = append(s.podPorts[req.PodUid], stableKey)
	s.mu.Unlock()

	return &nodev1.AddPortResponse{PortId: stableKey}, nil
}

// DeletePort cleans up OVS ports and cache entries.
// Deletes all OVS ports tracked for the pod, then removes netns and port caches.
func (s *NodeAgentServer) DeletePort(_ context.Context, req *nodev1.DeletePortRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	for _, p := range s.podPorts[req.PodUid] {
		s.delVethWithFlows(p)
	}
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
// Retries until the pod appears in the controller-runtime cache to handle the CNI/cache
// timing race where the pod object is not yet visible when CNI runs.
func (s *NodeAgentServer) GetPodAnnotation(
	ctx context.Context,
	req *nodev1.GetPodAnnotationRequest,
) (*nodev1.GetPodAnnotationResponse, error) {
	s.k8sMu.RLock()
	k8s := s.k8s
	s.k8sMu.RUnlock()
	if k8s == nil {
		return nil, fmt.Errorf("node-agent: not ready")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		var pod corev1.Pod
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &pod); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			if time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			return &nodev1.GetPodAnnotationResponse{Found: false}, nil
		}
		val, found := pod.Annotations[req.Key]
		return &nodev1.GetPodAnnotationResponse{Value: val, Found: found}, nil
	}
}

// StartGRPCServer starts the NodeAgent gRPC server on a Unix socket.
func StartGRPCServer(sockPath string, srv *NodeAgentServer) (*grpc.Server, error) {
	if err := secureSocketDir(filepath.Dir(sockPath)); err != nil {
		return nil, err
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

// secureSocketDir makes the directory of the node-agent socket private to root (0700), also when it already exists
// with looser permissions (a hostPath directory is created 0755): the socket drives pod networking on the node.
func secureSocketDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	return nil
}
