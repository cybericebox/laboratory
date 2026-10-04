package cnigate

import (
	_ "github.com/cybericebox/laboratory/pkg/runtime"

	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cybericebox/laboratory/internal/names"
	nodev1 "github.com/cybericebox/laboratory/pkg/rpc/node/v1"
)

// defaultAgentSocket matches install-cni's GRPC_SOCK default; the conflist
// normally sets agentSocket explicitly, this is only the fallback.
const defaultAgentSocket = "/run/cybericebox/node-agent.sock"

// cniLogFile receives cni-gate diagnostics. CNI stdout is reserved for the result
// JSON, so all logging goes to this file (and stderr, captured by the kubelet).
const cniLogFile = "/var/log/cni-gate.log"

// cniLogMaxBytes caps the diagnostics file: there is no logrotate for a CNI
// binary, so the file is truncated (old entries dropped) once it grows past this.
const cniLogMaxBytes = 10 << 20 // 10 MiB

func logf(format string, a ...interface{}) {
	line := time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + " " + fmt.Sprintf(format, a...) + "\n"
	fmt.Fprint(os.Stderr, line)
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(cniLogFile); err == nil && st.Size() > cniLogMaxBytes {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	if f, err := os.OpenFile(cniLogFile, flags, 0644); err == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
}

// NetConf is the CNI config for cni-gate.
type NetConf struct {
	cnitypes.NetConf
	Delegate    map[string]interface{} `json:"delegate,omitempty"`
	AgentSocket string                 `json:"agentSocket,omitempty"`
}

func Run() {
	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:    cmdADD,
		Del:    cmdDEL,
		Check:  cmdCHECK,
		GC:     nil,
		Status: nil,
	}, version.All, "cni-gate")
}

func cmdADD(args *skel.CmdArgs) error {
	conf, err := loadConf(args.StdinData)
	if err != nil {
		logf("ADD loadConf error: %v", err)
		return err
	}

	ns, name, uid := parsePodArgs(args.Args)
	logf("ADD pod=%s/%s uid=%s netns=%s ifname=%s", ns, name, uid, args.Netns, args.IfName)
	if ns == "" || name == "" {
		logf("ADD no pod args → delegateReal")
		return delegateReal(conf)
	}

	conn, client, err := dialAgent(conf)
	if err != nil {
		logf("ADD dialAgent error: %v", err)
		return err
	}
	defer func() {
		if err = conn.Close(); err != nil {
			logf("ADD conn.Close error: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.SetupNetworks(
		ctx, &nodev1.SetupNetworksRequest{
			Namespace: ns,
			Name:      name,
			PodUid:    uid,
			NetnsPath: args.Netns,
		},
	)
	if err != nil {
		logf("ADD SetupNetworks error pod=%s/%s: %v", ns, name, err)
		return fmt.Errorf("SetupNetworks: %w", err)
	}
	logf("ADD SetupNetworks resp pod=%s/%s defaultNetwork=%q", ns, name, resp.DefaultNetwork)

	var result *cniv1.Result

	switch resp.DefaultNetwork {
	case "real", names.DefaultEth0:
		// Regular pod or explicit eth0: delegate to k8s CNI normally.
		logf("ADD pod=%s/%s branch=REAL delegate eth0 to k8s CNI", ns, name)
		dt := delegateType(conf)
		if dt == "" {
			return fmt.Errorf("cni-gate: delegate.type required")
		}
		delegateResult, err := invoke.DelegateAdd(context.Background(), dt, marshalDelegate(conf), nil)
		if err != nil {
			logf("ADD pod=%s/%s delegate ADD error: %v", ns, name, err)
			return fmt.Errorf("delegate ADD: %w", err)
		}
		result, err = cniv1.NewResultFromResult(delegateResult)
		if err != nil {
			return fmt.Errorf("convert result: %w", err)
		}

	case "stub":
		// Device pod: OVS interfaces already wired by node-agent; no default network.
		logf("ADD pod=%s/%s branch=STUB no default network, stub eth0 only", ns, name)
		result = &cniv1.Result{CNIVersion: conf.CNIVersion}

	default:
		// Access-port pod: delegate k8s CNI to the named iface.
		accessIface := resp.DefaultNetwork
		logf("ADD pod=%s/%s branch=ACCESS delegate to iface=%s", ns, name, accessIface)
		dt := delegateType(conf)
		if dt == "" {
			return fmt.Errorf("cni-gate: delegate.type required for access port")
		}
		orig := os.Getenv("CNI_IFNAME")
		_ = os.Setenv("CNI_IFNAME", accessIface)
		delegateResult, delegateErr := invoke.DelegateAdd(context.Background(), dt, marshalDelegate(conf), nil)
		_ = os.Setenv("CNI_IFNAME", orig)
		if delegateErr != nil {
			logf("ADD pod=%s/%s delegate ADD for %s error: %v", ns, name, accessIface, delegateErr)
			return fmt.Errorf("delegate ADD for %s: %w", accessIface, delegateErr)
		}
		result, err = cniv1.NewResultFromResult(delegateResult)
		if err != nil {
			return fmt.Errorf("convert result: %w", err)
		}
	}

	// Kubernetes requires eth0 with an IP. Add a stub eth0 if the delegated
	// result has none (device/access pods), or attach an IP to a bare eth0.
	result = ensureEth0(result, args.Netns)
	logf("ADD pod=%s/%s done interfaces=%d ips=%d", ns, name, len(result.Interfaces), len(result.IPs))

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

	// Best-effort annotation check to decide whether DelegateDel is needed.
	// Errors are ignored: DEL must not fail the pod teardown.
	defaultIface, hasAnnotation, _ := getPodAnnotation(conf, args.Args, names.AnnotationDefaultNetwork)

	switch {
	case !hasAnnotation || defaultIface == names.DefaultEth0:
		return invoke.DelegateDel(context.Background(), delegateType(conf), marshalDelegate(conf), nil)
	case defaultIface != "":
		orig := os.Getenv("CNI_IFNAME")
		_ = os.Setenv("CNI_IFNAME", defaultIface)
		err := invoke.DelegateDel(context.Background(), delegateType(conf), marshalDelegate(conf), nil)
		_ = os.Setenv("CNI_IFNAME", orig)
		return err
	}
	return nil
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

// delegateReal delegates the full CNI ADD to the k8s CNI plugin and returns its result.
func delegateReal(conf *NetConf) error {
	dt := delegateType(conf)
	if dt == "" {
		return nil
	}
	delegateResult, err := invoke.DelegateAdd(context.Background(), dt, marshalDelegate(conf), nil)
	if err != nil {
		return fmt.Errorf("delegate ADD: %w", err)
	}
	result, err := cniv1.NewResultFromResult(delegateResult)
	if err != nil {
		return fmt.Errorf("convert result: %w", err)
	}
	return cnitypes.PrintResult(result, conf.CNIVersion)
}

func dialAgent(conf *NetConf) (*grpc.ClientConn, nodev1.NodeAgentClient, error) {
	socketPath := conf.AgentSocket
	if socketPath == "" {
		socketPath = defaultAgentSocket
	}
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial node-agent %s: %w", socketPath, err)
	}
	return conn, nodev1.NewNodeAgentClient(conn), nil
}

// getPodAnnotation fetches a pod annotation via the node-agent gRPC socket.
// Used by cmdDEL (best-effort, errors are ignored by the caller).
func getPodAnnotation(conf *NetConf, cniArgs, key string) (value string, found bool, err error) {
	conn, client, err := dialAgent(conf)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = conn.Close() }()

	ns, name, _ := parsePodArgs(cniArgs)
	if ns == "" || name == "" {
		return "", false, nil
	}

	resp, err := client.GetPodAnnotation(
		context.Background(),
		&nodev1.GetPodAnnotationRequest{Namespace: ns, Name: name, Key: key},
	)
	if err != nil {
		return "", false, fmt.Errorf("GetPodAnnotation %s/%s: %w", ns, name, err)
	}
	return resp.Value, resp.Found, nil
}

// parsePodArgs extracts K8S_POD_NAMESPACE, K8S_POD_NAME, and K8S_POD_UID from CNI_ARGS.
func parsePodArgs(cniArgs string) (namespace, name, uid string) {
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
		case "K8S_POD_UID":
			uid = kv[1]
		}
	}
	return
}

// ensureEth0 guarantees the CNI result has an "eth0" interface that owns at least
// one IP. Kubernetes/containerd require eth0 to exist with an IP to persist
// NetworkInfo, and containerd's selectPodIP walks sandbox interfaces in order,
// so pod.status.podIP comes from the IP entry pointing at eth0. Cases:
//   - eth0 present with an IP  → unchanged.
//   - eth0 present without IP  → attach a stub IP (127.0.0.1/32) to it.
//   - eth0 absent, result has a real IP (access-port pod) → prepend a logical
//     eth0 and redirect the first real IP entry to it, so Kubernetes reports the
//     real IP as podIP — NOT the stub. The IP still physically lives on the
//     access interface; the redirect is result metadata only.
//   - eth0 absent, no IPs (device pod) → prepend a stub eth0 + 127.0.0.1/32.
func ensureEth0(base *cniv1.Result, netns string) *cniv1.Result {
	_, stub, _ := net.ParseCIDR("127.0.0.1/32")

	eth0Idx := -1
	for i, iface := range base.Interfaces {
		if iface.Name == names.DefaultEth0 {
			eth0Idx = i
			break
		}
	}

	if eth0Idx >= 0 {
		for _, ip := range base.IPs {
			if ip.Interface != nil && *ip.Interface == eth0Idx {
				return base // eth0 already owns an IP
			}
		}
		base.IPs = append(base.IPs, &cniv1.IPConfig{Interface: cniv1.Int(eth0Idx), Address: *stub})
		return base
	}

	base.Interfaces = append([]*cniv1.Interface{{Name: names.DefaultEth0, Sandbox: netns}}, base.Interfaces...)
	if len(base.IPs) > 0 {
		// Redirect the delegate's primary IP to eth0 (index 0); shift the rest.
		for i, ipc := range base.IPs {
			if i == 0 {
				ipc.Interface = new(0)
				continue
			}
			if ipc.Interface != nil {
				ipc.Interface = new(*ipc.Interface + 1)
			}
		}
		return base
	}
	base.IPs = []*cniv1.IPConfig{{Interface: new(int), Address: *stub}}
	return base
}

func init() {
	if os.Getenv("CNI_PATH") == "" {
		_ = os.Setenv("CNI_PATH", "/opt/cni/bin")
	}
}
