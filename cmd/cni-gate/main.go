package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	annotationDefault  = "network.cybericebox.com/default"
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

	annotation, hasAnnotation, err := getPodAnnotation(conf, args.Args, annotationDefault)
	if err != nil {
		return err
	}

	skipDelegate, targetIface := parseDefaultAnnotation(annotation, !hasAnnotation)

	var result *cniv1.Result

	if skipDelegate {
		if err := createDummyEth0(args.Netns); err != nil {
			return fmt.Errorf("create dummy eth0: %w", err)
		}
		result = &cniv1.Result{CNIVersion: conf.CNIVersion}
	} else {
		dt := delegateType(conf)
		if dt == "" {
			return fmt.Errorf("cni-gate: delegate.type is required when annotation is not empty-string")
		}
		delegateResult, err := invoke.DelegateAdd(context.Background(), dt, marshalDelegate(conf), nil)
		if err != nil {
			return fmt.Errorf("delegate ADD: %w", err)
		}
		result, err = cniv1.NewResultFromResult(delegateResult)
		if err != nil {
			return fmt.Errorf("convert delegate result: %w", err)
		}

		if targetIface != "eth0" && targetIface != "" {
			if err := renameIface(args.Netns, "eth0", targetIface); err != nil {
				return fmt.Errorf("rename eth0→%s: %w", targetIface, err)
			}
			if err := createDummyEth0(args.Netns); err != nil {
				return fmt.Errorf("create dummy eth0 after rename: %w", err)
			}
		}
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
	return invoke.DelegateDel(context.Background(), delegateType(conf), marshalDelegate(conf), nil)
}

// parseDefaultAnnotation interprets the network.cybericebox.com/default annotation.
// noAnnotation=true means the key was absent → default eth0 behaviour.
// annotation="" (explicitly set) → skipDelegate=true (no k8s network).
// annotation="eth0" or noAnnotation → skipDelegate=false, targetIface="eth0".
// annotation="custom" → skipDelegate=false, targetIface="custom".
func parseDefaultAnnotation(annotation string, noAnnotation bool) (skipDelegate bool, targetIface string) {
	if noAnnotation || annotation == "eth0" {
		return false, "eth0"
	}
	if annotation == "" {
		return true, ""
	}
	return false, annotation
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
	b, _ := json.Marshal(conf.Delegate)
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

func createDummyEth0(netnsPath string) error {
	if err := execInNetns(netnsPath, "ip", "link", "add", "eth0", "type", "dummy"); err != nil {
		return err
	}
	return execInNetns(netnsPath, "ip", "link", "set", "eth0", "up")
}

func renameIface(netnsPath, oldName, newName string) error {
	return execInNetns(netnsPath, "ip", "link", "set", oldName, "name", newName)
}

func execInNetns(netnsPath, cmd string, args ...string) error {
	allArgs := append([]string{"netns", "exec", netnsPath, cmd}, args...)
	out, err := exec.Command("ip", allArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", cmd, args, err, out)
	}
	return nil
}

func init() {
	if os.Getenv("CNI_PATH") == "" {
		_ = os.Setenv("CNI_PATH", "/opt/cni/bin")
	}
}
