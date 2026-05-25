package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	nodev1 "github.com/cybericebox/laboratory/api/node/v1"
)

const (
	annotationNetworks = "network.cybericebox.com/networks"
	defaultGRPCSock    = "/run/cybericebox/node-agent.sock"
	defaultKubeconfig  = "/etc/cni/net.d/cybericebox-kubeconfig.conf"
)

// NetConf is the CNI config for cni-ovs.
type NetConf struct {
	cnitypes.NetConf
	GRPCSock   string `json:"grpcSock,omitempty"`
	Kubeconfig string `json:"kubeconfig,omitempty"`
}

// netAttachment represents a single connection@iface entry.
type netAttachment struct {
	Connection string
	Interface  string
}

func main() {
	skel.PluginMain(cmdADD, cmdCHECK, cmdDEL, version.All, "cni-ovs")
}

func cmdADD(args *skel.CmdArgs) error {
	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	ns, podName, podUID := parsePodArgs(args.Args)
	if ns == "" || podName == "" {
		return nil
	}

	annotation, err := getPodAnnotation(conf, ns, podName, annotationNetworks)
	if err != nil {
		return err
	}

	attachments := parseNetworksAnnotation(annotation)
	if len(attachments) == 0 {
		return nil
	}

	grpcConn, err := grpc.NewClient(
		"unix://"+grpcSock(conf),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("connect node-agent gRPC: %w", err)
	}
	defer grpcConn.Close()

	client := nodev1.NewNodeAgentClient(grpcConn)

	var added int
	for _, att := range attachments {
		_, err := client.AddPort(context.Background(), &nodev1.AddPortRequest{
			PodUid:        podUID,
			Connection:    att.Connection,
			InterfaceName: att.Interface,
			Namespace:     ns,
			NetnsPath:     args.Netns,
		})
		if err != nil {
			if added > 0 {
				_, _ = client.DeletePort(context.Background(), &nodev1.DeletePortRequest{PodUid: podUID})
			}
			return fmt.Errorf("AddPort conn=%q iface=%q: %w", att.Connection, att.Interface, err)
		}
		added++
	}

	return nil
}

func cmdCHECK(_ *skel.CmdArgs) error { return nil }

func cmdDEL(args *skel.CmdArgs) error {
	conf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	_, _, podUID := parsePodArgs(args.Args)
	if podUID == "" {
		return nil
	}

	grpcConn, err := grpc.NewClient(
		"unix://"+grpcSock(conf),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil // best-effort
	}
	defer grpcConn.Close()

	client := nodev1.NewNodeAgentClient(grpcConn)
	_, _ = client.DeletePort(context.Background(), &nodev1.DeletePortRequest{PodUid: podUID})
	return nil
}

// parseNetworksAnnotation parses "conn1@eth0,conn2@eth1" into netAttachment slices.
func parseNetworksAnnotation(annotation string) []netAttachment {
	if annotation == "" {
		return nil
	}
	var result []netAttachment
	for _, entry := range strings.Split(annotation, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		at := strings.Index(entry, "@")
		if at < 0 {
			continue
		}
		result = append(result, netAttachment{
			Connection: entry[:at],
			Interface:  entry[at+1:],
		})
	}
	return result
}

func loadConf(data []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(data, conf); err != nil {
		return nil, fmt.Errorf("parse CNI config: %w", err)
	}
	return conf, nil
}

func grpcSock(conf *NetConf) string {
	if conf.GRPCSock != "" {
		return conf.GRPCSock
	}
	return defaultGRPCSock
}

func getPodAnnotation(conf *NetConf, namespace, podName, key string) (string, error) {
	kubeconfigPath := conf.Kubeconfig
	if kubeconfigPath == "" {
		kubeconfigPath = defaultKubeconfig
	}
	k8sCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("build kubeconfig: %w", err)
	}
	k8s, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		return "", fmt.Errorf("k8s client: %w", err)
	}
	pod, err := k8s.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get pod %s/%s: %w", namespace, podName, err)
	}
	return pod.Annotations[key], nil
}

// parsePodArgs extracts namespace, name, and UID from CNI_ARGS.
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
