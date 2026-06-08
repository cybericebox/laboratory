package main

import (
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nodev1 "github.com/cybericebox/laboratory/pkg/rpc/node/v1"
)

const (
	annotationNetworks = "network.cybericebox.com/networks"
	defaultNetworkName = "default"
	defaultAgentSocket = "/run/openvswitch/node-agent.sock"
)

// NetConf is the CNI config for cni-gate.
type NetConf struct {
	cnitypes.NetConf
	Delegate    map[string]interface{} `json:"delegate,omitempty"`
	AgentSocket string                 `json:"agentSocket,omitempty"`
}

func main() {
	skel.PluginMain(cmdADD, cmdCHECK, cmdDEL, version.All, "cni-gate")
}

func cmdADD(args *skel.CmdArgs) error {
	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	annotation, hasAnnotation, err := getPodAnnotation(conf, args.Args, annotationNetworks)
	if err != nil {
		return err
	}

	accessIface := findDefaultIface(annotation) // "" if no iface@default entry

	var result *cniv1.Result

	switch {
	case !hasAnnotation || accessIface == "eth0":
		// Regular pod (no annotation) or explicit eth0@default → delegate to k8s CNI normally.
		dt := delegateType(conf)
		if dt == "" {
			return fmt.Errorf("cni-gate: delegate.type is required for pods with default network")
		}
		delegateResult, err := invoke.DelegateAdd(context.Background(), dt, marshalDelegate(conf), nil)
		if err != nil {
			return fmt.Errorf("delegate ADD: %w", err)
		}
		result, err = cniv1.NewResultFromResult(delegateResult)
		if err != nil {
			return fmt.Errorf("convert delegate result: %w", err)
		}

	case accessIface != "":
		// Device pod with external access (e.g. "accessport@default"):
		// delegate to k8s CNI for the named access interface, then prepend
		// eth0 stub so containerd's selectPodIP can find a sandbox interface.
		// The real IP is moved to IPs[0] so Kubernetes reports the correct
		// podIP (not 127.0.0.1 from the stub).
		dt := delegateType(conf)
		if dt == "" {
			return fmt.Errorf("cni-gate: delegate.type is required for access port")
		}
		orig := os.Getenv("CNI_IFNAME")
		os.Setenv("CNI_IFNAME", accessIface)
		delegateResult, delegateErr := invoke.DelegateAdd(context.Background(), dt, marshalDelegate(conf), nil)
		os.Setenv("CNI_IFNAME", orig)
		if delegateErr != nil {
			return fmt.Errorf("delegate ADD for %s: %w", accessIface, delegateErr)
		}
		result, err = cniv1.NewResultFromResult(delegateResult)
		if err != nil {
			return fmt.Errorf("convert delegate result: %w", err)
		}
		// extendWithEth0 prepends eth0 stub (Interfaces[0], Sandbox=netns) and
		// adds 127.0.0.1 as IPs[0] pointing to it. selectPodIP in containerd
		// iterates sandbox interfaces in order, so it picks eth0 first and uses
		// IPs[0] (127.0.0.1) as podIP. To make it use the real IP instead,
		// redirect the real IP entry (IPs[1+]) to point to eth0 (index 0) and
		// drop the stub IP entry.
		result = extendWithEth0(result, args.Netns)
		// Find the real IP (points to shifted interface index ≥1) and redirect
		// it to eth0 (index 0). Keep only that one IP entry.
		for _, ip := range result.IPs {
			if ip.Interface != nil && *ip.Interface != 0 {
				ip.Interface = cniv1.Int(0) // attribute real IP to eth0
				result.IPs = []*cniv1.IPConfig{ip}
				break
			}
		}

	default:
		// Device pod: annotation present, no @default entry.
		// Return stub eth0 so containerd stores NetworkInfo; real interfaces wired by node-agent.
		result = extendWithEth0(&cniv1.Result{CNIVersion: conf.CNIVersion}, args.Netns)
	}

	return cnitypes.PrintResult(result, conf.CNIVersion)
}

func cmdCHECK(_ *skel.CmdArgs) error { return nil }

func cmdDEL(args *skel.CmdArgs) error {
	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}
	if len(conf.Delegate) == 0 {
		return nil
	}

	annotation, hasAnnotation, _ := getPodAnnotation(conf, args.Args, annotationNetworks)
	accessIface := findDefaultIface(annotation)

	switch {
	case !hasAnnotation || accessIface == "eth0":
		return invoke.DelegateDel(context.Background(), delegateType(conf), marshalDelegate(conf), nil)
	case accessIface != "":
		orig := os.Getenv("CNI_IFNAME")
		os.Setenv("CNI_IFNAME", accessIface)
		err := invoke.DelegateDel(context.Background(), delegateType(conf), marshalDelegate(conf), nil)
		os.Setenv("CNI_IFNAME", orig)
		return err
	}
	return nil
}

// findDefaultIface returns the interface name bound to @default in the annotation,
// or "" if no such entry exists. Used to detect whether a pod needs Kubernetes
// network (and on which interface) vs. a dummy eth0 stub.
func findDefaultIface(annotation string) string {
	for _, entry := range strings.Split(annotation, ",") {
		iface, name, ok := strings.Cut(strings.TrimSpace(entry), "@")
		if ok && iface != "" && name == defaultNetworkName {
			return iface
		}
	}
	return ""
}

func loadConf(data []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(data, conf); err != nil {
		return nil, fmt.Errorf("parse CNI config: %w", err)
	}
	return conf, nil
}

func delegateType(conf *NetConf) string {
	t, _ := conf.Delegate["type"].(string)
	return t
}

func marshalDelegate(conf *NetConf) []byte {
	d := make(map[string]interface{}, len(conf.Delegate)+2)
	for k, v := range conf.Delegate {
		d[k] = v
	}
	d["name"] = conf.Name
	d["cniVersion"] = conf.CNIVersion
	b, _ := json.Marshal(d)
	return b
}

// getPodAnnotation fetches a pod annotation via the node-agent gRPC socket.
// The CNI plugin must not access the Kubernetes API directly.
func getPodAnnotation(conf *NetConf, cniArgs, key string) (value string, found bool, err error) {
	socketPath := conf.AgentSocket
	if socketPath == "" {
		socketPath = defaultAgentSocket
	}

	conn, err := grpc.NewClient("unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", false, fmt.Errorf("dial node-agent %s: %w", socketPath, err)
	}
	defer conn.Close()

	ns, name := parsePodArgs(cniArgs)
	if ns == "" || name == "" {
		return "", false, nil
	}

	resp, err := nodev1.NewNodeAgentClient(conn).GetPodAnnotation(context.Background(),
		&nodev1.GetPodAnnotationRequest{Namespace: ns, Name: name, Key: key})
	if err != nil {
		return "", false, fmt.Errorf("GetPodAnnotation %s/%s: %w", ns, name, err)
	}
	return resp.Value, resp.Found, nil
}

// parsePodArgs extracts K8S_POD_NAMESPACE and K8S_POD_NAME from CNI_ARGS.
// CNI_ARGS format: "K8S_POD_NAMESPACE=ns;K8S_POD_NAME=name;..."
func parsePodArgs(cniArgs string) (namespace, name string) {
	for _, part := range strings.Split(cniArgs, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "K8S_POD_NAMESPACE":
			namespace = kv[1]
		case "K8S_POD_NAME":
			name = kv[1]
		}
	}
	return
}

// extendWithEth0 prepends a stub eth0 interface (127.0.0.1/32) to base, shifting
// existing IP interface indices by +1. containerd requires at least one interface+IP
// in the CNI result to persist NetworkInfo; without it teardown fails with
// "failed to find network info for sandbox". The eth0 is a logical stub only —
// no interface is created in the pod netns; real lab interfaces are wired by node-agent.
func extendWithEth0(base *cniv1.Result, netns string) *cniv1.Result {
	_, stub, _ := net.ParseCIDR("127.0.0.1/32")

	// Shift existing IP interface pointers to account for the new eth0 at index 0.
	for _, ipc := range base.IPs {
		if ipc.Interface != nil {
			shifted := *ipc.Interface + 1
			ipc.Interface = &shifted
		}
	}

	base.Interfaces = append([]*cniv1.Interface{{Name: "eth0", Sandbox: netns}}, base.Interfaces...)
	base.IPs = append([]*cniv1.IPConfig{{Interface: cniv1.Int(0), Address: *stub}}, base.IPs...)
	return base
}


func init() {
	if os.Getenv("CNI_PATH") == "" {
		_ = os.Setenv("CNI_PATH", "/opt/cni/bin")
	}
}
