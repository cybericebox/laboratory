package cnigate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/skel"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/types/create"
	"github.com/containernetworking/cni/pkg/utils"
	"github.com/cybericebox/laboratory/internal/names"
)

// These are the identity/config fields of libcni's cniCacheV1 receipt. Read the
// single canonical runtime key, not a directory search or an allocator record.
type delCache struct {
	Kind        string          `json:"kind"`
	ContainerID string          `json:"containerId"`
	NetworkName string          `json:"networkName"`
	IfName      string          `json:"ifName"`
	NetNS       string          `json:"netns"`
	Config      []byte          `json:"config"`
	CniArgs     [][2]string     `json:"cniArgs"`
	Result      json.RawMessage `json:"result"`
}

func delegateForDEL(conf *NetConf, args *skel.CmdArgs) (*NetConf, string, bool, error) {
	if err := utils.ValidateNetworkName(conf.Name); err != nil {
		return nil, "", false, err
	}
	if err := utils.ValidateContainerID(args.ContainerID); err != nil {
		return nil, "", false, err
	}
	if err := utils.ValidateInterfaceName(args.IfName); err != nil {
		return nil, "", false, err
	}
	path := filepath.Join(libcni.CacheDir, "results", conf.Name+"-"+args.ContainerID+"-"+args.IfName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A partial ADD or idempotent DEL may have no runtime receipt. Keep the
		// existing selector; this cannot recover a lost legacy allocation domain.
		return conf, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("read ADD receipt: %w", err)
	}
	var cache delCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, "", false, fmt.Errorf("parse ADD receipt: %w", err)
	}
	if cache.Kind != libcni.CNICacheV1 || cache.ContainerID != args.ContainerID || cache.NetworkName != conf.Name || cache.IfName != args.IfName {
		return nil, "", false, fmt.Errorf("ADD receipt identity does not match DEL")
	}
	if cache.NetNS != "" && args.Netns != "" && cache.NetNS != args.Netns {
		return nil, "", false, fmt.Errorf("ADD receipt network namespace does not match DEL")
	}
	cachedArgs := make([]string, 0, len(cache.CniArgs))
	for _, arg := range cache.CniArgs {
		cachedArgs = append(cachedArgs, arg[0]+"="+arg[1])
	}
	oldUID, err := receiptPodUID(strings.Join(cachedArgs, ";"))
	if err != nil {
		return nil, "", false, err
	}
	uid, err := receiptPodUID(args.Args)
	if err != nil || uid != oldUID {
		return nil, "", false, fmt.Errorf("ADD receipt Pod UID does not match DEL")
	}
	list, err := libcni.ConfListFromBytes(cache.Config)
	if err != nil || list.Name != conf.Name || len(list.Plugins) != 1 || list.Plugins[0].Network.Type != "cni-gate" {
		return nil, "", false, fmt.Errorf("ADD receipt has no unambiguous cni-gate config")
	}
	plugin, err := libcni.InjectConf(list.Plugins[0], map[string]interface{}{"name": list.Name, "cniVersion": list.CNIVersion})
	if err != nil {
		return nil, "", false, fmt.Errorf("restore ADD config: %w", err)
	}
	old, err := loadConf(plugin.Bytes)
	if err != nil || delegateType(old) == "" {
		return nil, "", false, fmt.Errorf("ADD receipt has no delegate")
	}
	iface, err := receiptDelegateIface(cache, args.IfName, strings.Join(cachedArgs, ";"))
	if err != nil {
		return nil, "", false, err
	}
	restored := *conf // control-plane socket/settings stay current
	restored.Delegate = old.Delegate
	restored.CNIVersion = old.CNIVersion
	restored.PreserveDelegateName = old.PreserveDelegateName
	return &restored, iface, true, nil
}

func receiptPodUID(args string) (string, error) {
	uid := ""
	seen := false
	for _, arg := range strings.Split(args, ";") {
		key, value, ok := strings.Cut(arg, "=")
		if !ok || key != "K8S_POD_UID" {
			continue
		}
		if seen && uid != value {
			return "", fmt.Errorf("ambiguous Pod UID in ADD/DEL args")
		}
		uid = value
		seen = true
	}
	return uid, nil
}

func receiptDelegateIface(cache delCache, outerIface, cniArgs string) (string, error) {
	ns, pod, _ := parsePodArgs(cniArgs)
	if ns == "" || pod == "" {
		// cmdADD's direct delegateReal path used the runtime's exact ifName.
		return outerIface, nil
	}
	// The upstream version converter dereferences interface/IP entries. Reject
	// null receipt entries before invoking it rather than allowing a panic.
	var shape struct {
		Interfaces []json.RawMessage `json:"interfaces"`
		IPs        []json.RawMessage `json:"ips"`
	}
	if err := json.Unmarshal(cache.Result, &shape); err != nil {
		return "", fmt.Errorf("parse ADD interface receipt: %w", err)
	}
	for _, entries := range [][]json.RawMessage{shape.Interfaces, shape.IPs} {
		for _, entry := range entries {
			if bytes.Equal(bytes.TrimSpace(entry), []byte("null")) {
				return "", fmt.Errorf("malformed ADD interface/IP receipt")
			}
		}
	}
	result, err := create.CreateFromBytes(cache.Result)
	if err != nil {
		return "", fmt.Errorf("parse ADD interface receipt: %w", err)
	}
	converted, err := cniv1.NewResultFromResult(result)
	if err != nil {
		return "", fmt.Errorf("convert ADD interface receipt: %w", err)
	}
	var sandbox []*cniv1.Interface
	seen := map[string]bool{}
	for _, iface := range converted.Interfaces {
		if iface == nil {
			return "", fmt.Errorf("malformed ADD interface receipt")
		}
		if iface.Sandbox == "" {
			continue
		}
		if seen[iface.Name] || (cache.NetNS != "" && iface.Sandbox != cache.NetNS) || utils.ValidateInterfaceName(iface.Name) != nil {
			return "", fmt.Errorf("ambiguous ADD interface receipt")
		}
		seen[iface.Name] = true
		sandbox = append(sandbox, iface)
	}
	for _, ip := range converted.IPs {
		if ip == nil {
			return "", fmt.Errorf("malformed ADD IP receipt")
		}
	}
	if len(sandbox) == 1 && sandbox[0].Name == outerIface {
		if len(converted.Interfaces) == 1 && sandbox[0].Name == names.DefaultEth0 && sandbox[0].Mac == "" && len(converted.IPs) == 1 && converted.IPs[0].Interface != nil && *converted.IPs[0].Interface == 0 && converted.IPs[0].Address.String() == "127.0.0.1/32" {
			// The original stub branch had no delegated network ADD.
			return "", nil
		}
		return outerIface, nil
	}
	// ensureEth0 prepends a logical eth0 for an access-port result. Its MAC is
	// empty; the sole physical sandbox interface retains its delegate MAC.
	if len(sandbox) == 2 && len(converted.Interfaces) > 0 && sandbox[0] == converted.Interfaces[0] && sandbox[0].Name == names.DefaultEth0 && sandbox[0].Mac == "" && sandbox[1].Name != names.DefaultEth0 && sandbox[1].Mac != "" && sandbox[0].Sandbox == sandbox[1].Sandbox {
		return sandbox[1].Name, nil
	}
	return "", fmt.Errorf("ADD receipt has no unambiguous delegate interface")
}
