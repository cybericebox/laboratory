package cnigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/types/create"
	"github.com/containernetworking/cni/pkg/version"
	nodev1 "github.com/cybericebox/laboratory/pkg/rpc/node/v1"
	"google.golang.org/grpc"
)

const runtimeDELCID = "53d0ab4fbbee9b51a8cc3da59491b322b99fb393171d4d8fb863c105b8155acb"
const runtimeDELUID = "89579a20-97a3-4019-9fd2-dcfca74ff368"

// libcni really caches ADD's list, invokes DEL with a different current list,
// and removes its receipt after the callback. The delegate is the actual
// upstream host-local binary, writing its real SID/ifName allocation receipt.
func TestRuntimeDELUsesADDIdentityWithCurrentConfig(t *testing.T) {
	for _, tc := range []struct {
		name, oldName, wantDomain string
		preserve                  bool
	}{
		{"legacy-unnamed", "", "cybericebox", false},
		{"legacy-named", "kindnet", "cybericebox", false},
		{"new-named", "kindnet", "kindnet", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cni, cacheDir, dataDir := runtimeDELFixture(t)
			rt := &libcni.RuntimeConf{ContainerID: runtimeDELCID, NetNS: t.TempDir(), IfName: "eth0", Args: [][2]string{{"IgnoreUnknown", "1"}, {"K8S_POD_UID", runtimeDELUID}}}
			old := runtimeDELList(t, dataDir, tc.oldName, tc.preserve)
			result, err := cni.AddNetworkList(context.Background(), old, rt)
			if err != nil {
				t.Fatal(err)
			}
			converted, err := cniv1.NewResultFromResult(result)
			if err != nil || len(converted.IPs) != 1 {
				t.Fatalf("real host-local ADD result: %v %v", result, err)
			}
			lease := filepath.Join(dataDir, tc.wantDomain, converted.IPs[0].Address.IP.String())
			owner, err := os.ReadFile(lease)
			if err != nil || strings.TrimSpace(string(owner)) != runtimeDELCID+"\r\neth0" {
				t.Fatalf("ADD did not leave the exact allocator receipt: %q %v", owner, err)
			}
			cache := filepath.Join(cacheDir, "results", "cybericebox-"+runtimeDELCID+"-eth0")
			if _, err := os.Stat(cache); err != nil {
				t.Fatal(err)
			}
			current := runtimeDELList(t, dataDir, "changed-current-domain", true)
			if err := cni.DelNetworkList(context.Background(), current, rt); err != nil {
				t.Fatal(err)
			}
			var callback struct {
				Present bool `json:"present"`
			}
			observed, err := os.ReadFile(filepath.Join(cacheDir, "del-callback.json"))
			if err != nil || json.Unmarshal(observed, &callback) != nil || !callback.Present {
				t.Fatalf("original cache absent during compiled plugin DEL callback: %s %v", observed, err)
			}
			if _, err := os.Stat(cache); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("libcni did not remove its cache after successful DEL: %v", err)
			}
			if _, err := os.Stat(lease); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("ADD's original %s SID/eth0 allocator receipt survived DEL with the current selector: %v", tc.wantDomain, err)
			}
		})
	}
}

func TestRuntimeDELRejectsForeignOrAmbiguousReceipts(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*delCache)
	}{
		{"wrong-container", func(c *delCache) { c.ContainerID = "other-container" }},
		{"wrong-interface", func(c *delCache) { c.IfName = "accessport" }},
		{"wrong-network", func(c *delCache) { c.NetworkName = "other-network" }},
		{"wrong-netns", func(c *delCache) { c.NetNS = "/other/netns" }},
		{"wrong-pod-uid", func(c *delCache) { c.CniArgs = [][2]string{{"K8S_POD_UID", "replacement-uid"}} }},
		{"missing-pod-uid", func(c *delCache) { c.CniArgs = nil }},
		{"ambiguous-pod-uid", func(c *delCache) { c.CniArgs = append(c.CniArgs, [2]string{"K8S_POD_UID", "replacement-uid"}) }},
		{"wrong-cache-kind", func(c *delCache) { c.Kind = "other-kind" }},
		{"invalid-config", func(c *delCache) { c.Config = []byte(`{`) }},
		{"multiple-gates", func(c *delCache) {
			c.Config = []byte(`{"name":"cybericebox","cniVersion":"0.3.1","plugins":[{"type":"cni-gate"},{"type":"cni-gate"}]}`)
		}},
		{"invalid-result", func(c *delCache) { c.CniArgs = runtimeDELPodArgs(); c.Result = json.RawMessage(`{}`) }},
		{"nil-interface", func(c *delCache) {
			c.CniArgs = runtimeDELPodArgs()
			c.Result = json.RawMessage(`{"cniVersion":"0.3.1","interfaces":[null]}`)
		}},
		{"nil-ip", func(c *delCache) {
			c.CniArgs = runtimeDELPodArgs()
			c.Result = json.RawMessage(`{"cniVersion":"0.3.1","interfaces":[{"name":"eth0","sandbox":"/test/ns"}],"ips":[null]}`)
		}},
		{"ambiguous-interfaces", func(c *delCache) {
			c.CniArgs = runtimeDELPodArgs()
			c.Result = json.RawMessage(`{"cniVersion":"0.3.1","interfaces":[{"name":"eth0","mac":"00:00:00:00:00:01","sandbox":"/test/ns"},{"name":"accessport","mac":"00:00:00:00:00:02","sandbox":"/test/ns"}]}`)
			c.NetNS = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cni, cacheDir, dataDir := runtimeDELFixture(t)
			rt := runtimeDELArgs(t)
			old := runtimeDELList(t, dataDir, "kindnet", false)
			result, err := cni.AddNetworkList(context.Background(), old, rt)
			if err != nil {
				t.Fatal(err)
			}
			converted, err := cniv1.NewResultFromResult(result)
			if err != nil {
				t.Fatal(err)
			}
			lease := filepath.Join(dataDir, "cybericebox", converted.IPs[0].Address.IP.String())
			owner, err := os.ReadFile(lease)
			if err != nil {
				t.Fatal(err)
			}
			path := runtimeDELCachePath(cacheDir)
			updateRuntimeDELCache(t, path, tc.edit)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			current := runtimeDELList(t, dataDir, "changed-current-domain", true)
			if err := cni.DelNetworkList(context.Background(), current, rt); err == nil {
				t.Fatal("untrusted receipt permitted a successful DEL")
			} else {
				var exited *exec.ExitError
				if !errors.As(err, &exited) || exited.ExitCode() != 1 {
					t.Fatalf("receipt did not produce a clean CNI refusal: %v", err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("untrusted receipt was removed or modified: %v", err)
			}
			afterOwner, err := os.ReadFile(lease)
			if err != nil || string(afterOwner) != string(owner) {
				t.Fatalf("untrusted receipt changed allocator owner: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, "changed-current-domain")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("untrusted receipt invoked current delegate: %v", err)
			}
		})
	}
}

func TestRuntimeDELMissingCacheDoesNotRecoverLegacyLease(t *testing.T) {
	cni, cacheDir, dataDir := runtimeDELFixture(t)
	rt := runtimeDELArgs(t)
	result, err := cni.AddNetworkList(context.Background(), runtimeDELList(t, dataDir, "", false), rt)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := cniv1.NewResultFromResult(result)
	if err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(dataDir, "cybericebox", converted.IPs[0].Address.IP.String())
	owner, err := os.ReadFile(lease)
	if err != nil {
		t.Fatal(err)
	}
	// Model a receipt already consumed by an earlier runtime DEL. No lease is edited.
	if err := os.Remove(runtimeDELCachePath(cacheDir)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := cni.DelNetworkList(context.Background(), runtimeDELList(t, dataDir, "changed-current-domain", true), rt); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(lease)
	if err != nil || string(after) != string(owner) {
		t.Fatalf("lost original receipt guessed or repaired a legacy lease: %v", err)
	}
}

func TestRuntimeDELRestoresPhysicalInterfaceWithoutCurrentAnnotation(t *testing.T) {
	for _, iface := range []string{"eth0", "accessport"} {
		t.Run(iface, func(t *testing.T) {
			cni, cacheDir, dataDir := runtimeDELFixture(t)
			rt := runtimeDELArgs(t)
			old := runtimeDELList(t, dataDir, "kindnet", false)
			result, err := cni.AddNetworkList(context.Background(), old, rt)
			if err != nil {
				t.Fatal(err)
			}
			converted, err := cniv1.NewResultFromResult(result)
			if err != nil {
				t.Fatal(err)
			}
			otherIfaceLease := filepath.Join(dataDir, "cybericebox", converted.IPs[0].Address.IP.String())
			if iface == "accessport" {
				converted = hostLocalRuntimeADD(t, old, rt, iface)
			}
			lease := filepath.Join(dataDir, "cybericebox", converted.IPs[0].Address.IP.String())
			converted.Interfaces = []*cniv1.Interface{{Name: "host-veth", Mac: "00:00:00:00:00:01"}, {Name: iface, Mac: "00:00:00:00:00:02", Sandbox: rt.NetNS}}
			converted.IPs[0].Interface = cniv1.Int(1)
			converted = ensureEth0(converted, rt.NetNS)
			resultJSON, err := converted.GetAsVersion("0.3.1")
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(resultJSON)
			if err != nil {
				t.Fatal(err)
			}
			updateRuntimeDELCache(t, runtimeDELCachePath(cacheDir), func(c *delCache) { c.CniArgs = runtimeDELPodArgs(); c.Result = encoded })
			rt.Args = runtimeDELPodArgs()
			current := runtimeDELList(t, dataDir, "changed-current-domain", true)
			agent, socket := runtimeDELEmptyAnnotationAgent(t)
			var currentJSON map[string]interface{}
			if err := json.Unmarshal(current.Bytes, &currentJSON); err != nil {
				t.Fatal(err)
			}
			currentJSON["plugins"].([]interface{})[0].(map[string]interface{})["agentSocket"] = socket
			data, err := json.Marshal(currentJSON)
			if err != nil {
				t.Fatal(err)
			}
			current, err = libcni.ConfListFromBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			if err := cni.DelNetworkList(context.Background(), current, rt); err != nil {
				t.Fatal(err)
			}
			if agent.calls.Load() != 0 {
				t.Fatal("authoritative receipt depended on a replacement/current Pod annotation")
			}
			if _, err := os.Stat(lease); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("original %s lease survived: %v", iface, err)
			}
			if iface != "eth0" {
				if _, err := os.Stat(otherIfaceLease); err != nil {
					t.Fatalf("different ifName lease was deleted: %v", err)
				}
			}
		})
	}
}

func TestRuntimeDELOriginalStubDoesNotDelegate(t *testing.T) {
	cni, cacheDir, dataDir := runtimeDELFixture(t)
	rt := runtimeDELArgs(t)
	rt.Args = runtimeDELPodArgs()
	agent, socket := runtimeDELEmptyAnnotationAgent(t)
	old := runtimeDELWithSocket(t, runtimeDELList(t, dataDir, "kindnet", false), socket)
	if _, err := cni.AddNetworkList(context.Background(), old, rt); err != nil {
		t.Fatal(err)
	}
	current := runtimeDELWithSocket(t, runtimeDELList(t, dataDir, "changed-current-domain", true), socket)
	if err := cni.DelNetworkList(context.Background(), current, rt); err != nil {
		t.Fatal(err)
	}
	if agent.setups.Load() != 1 || agent.calls.Load() != 0 {
		t.Fatal("stub DEL used the current annotation instead of the original ADD receipt")
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("original stub invoked an allocator: %v %v", entries, err)
	}
	if _, err := os.Stat(runtimeDELCachePath(cacheDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stub receipt retained after DEL: %v", err)
	}
}

func TestRuntimeDELRestoresOnlyAllocationConfig(t *testing.T) {
	cni, cacheDir, dataDir := runtimeDELFixture(t)
	rt := runtimeDELArgs(t)
	old := runtimeDELWithSocket(t, runtimeDELList(t, dataDir, "kindnet", false), "/old/agent.sock")
	if _, err := cni.AddNetworkList(context.Background(), old, rt); err != nil {
		t.Fatal(err)
	}
	current := runtimeDELWithSocket(t, runtimeDELList(t, dataDir, "changed-current-domain", true), "/current/agent.sock")
	plugin, err := libcni.InjectConf(current.Plugins[0], map[string]interface{}{"name": current.Name, "cniVersion": "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	conf, err := loadConf(plugin.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(conf)
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := libcni.CacheDir
	libcni.CacheDir = cacheDir
	t.Cleanup(func() { libcni.CacheDir = oldRoot })
	restored, iface, cached, err := delegateForDEL(conf, &skel.CmdArgs{ContainerID: rt.ContainerID, IfName: rt.IfName, Netns: rt.NetNS, Args: "IgnoreUnknown=1;K8S_POD_UID=" + runtimeDELUID})
	if err != nil || !cached || iface != "eth0" {
		t.Fatalf("restore original allocation: %v %q %v", cached, iface, err)
	}
	if restored.AgentSocket != "/current/agent.sock" || restored.Name != "cybericebox" || restored.CNIVersion != "0.3.1" || restored.PreserveDelegateName || restored.Delegate["name"] != "kindnet" {
		t.Fatalf("incorrect allocation/control settings: %+v", restored)
	}
	after, err := json.Marshal(conf)
	if err != nil || string(after) != string(before) {
		t.Fatal("current config was mutated")
	}
	// An empty current netns is a valid post-sandbox teardown.
	if _, _, _, err := delegateForDEL(conf, &skel.CmdArgs{ContainerID: rt.ContainerID, IfName: rt.IfName, Args: "IgnoreUnknown=1;K8S_POD_UID=" + runtimeDELUID}); err != nil {
		t.Fatal(err)
	}
}

func runtimeDELWithSocket(t *testing.T, list *libcni.NetworkConfigList, socket string) *libcni.NetworkConfigList {
	t.Helper()
	var conf map[string]interface{}
	if err := json.Unmarshal(list.Bytes, &conf); err != nil {
		t.Fatal(err)
	}
	conf["plugins"].([]interface{})[0].(map[string]interface{})["agentSocket"] = socket
	data, err := json.Marshal(conf)
	if err != nil {
		t.Fatal(err)
	}
	result, err := libcni.ConfListFromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func runtimeDELArgs(t *testing.T) *libcni.RuntimeConf {
	t.Helper()
	return &libcni.RuntimeConf{ContainerID: runtimeDELCID, NetNS: t.TempDir(), IfName: "eth0", Args: [][2]string{{"IgnoreUnknown", "1"}, {"K8S_POD_UID", runtimeDELUID}}}
}

func runtimeDELPodArgs() [][2]string {
	return [][2]string{{"IgnoreUnknown", "1"}, {"K8S_POD_UID", runtimeDELUID}, {"K8S_POD_NAMESPACE", "test-ns"}, {"K8S_POD_NAME", "test-pod"}}
}

func runtimeDELCachePath(dir string) string {
	return filepath.Join(dir, "results", "cybericebox-"+runtimeDELCID+"-eth0")
}

func updateRuntimeDELCache(t *testing.T, path string, edit func(*delCache)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c delCache
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	edit(&c)
	data, err = json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hostLocalRuntimeADD(t *testing.T, list *libcni.NetworkConfigList, rt *libcni.RuntimeConf, iface string) *cniv1.Result {
	t.Helper()
	plugin, err := libcni.InjectConf(list.Plugins[0], map[string]interface{}{"name": list.Name, "cniVersion": list.CNIVersion})
	if err != nil {
		t.Fatal(err)
	}
	conf, err := loadConf(plugin.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Getenv("CICE_HOST_LOCAL_TEST_BINARY"))
	cmd.Env = append(os.Environ(), "CNI_COMMAND=ADD", "CNI_CONTAINERID="+rt.ContainerID, "CNI_NETNS="+rt.NetNS, "CNI_IFNAME="+iface, "CNI_ARGS=IgnoreUnknown=1")
	cmd.Stdin = strings.NewReader(string(marshalDelegate(conf)))
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("real host-local ADD %s: %v %s", iface, err, output)
	}
	result, err := create.CreateFromBytes(output)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := cniv1.NewResultFromResult(result)
	if err != nil {
		t.Fatal(err)
	}
	return converted
}

type runtimeDELAnnotationAgent struct {
	nodev1.UnimplementedNodeAgentServer
	calls  atomic.Int32
	setups atomic.Int32
}

func (a *runtimeDELAnnotationAgent) SetupNetworks(context.Context, *nodev1.SetupNetworksRequest) (*nodev1.SetupNetworksResponse, error) {
	a.setups.Add(1)
	return &nodev1.SetupNetworksResponse{DefaultNetwork: "stub"}, nil
}

func (a *runtimeDELAnnotationAgent) GetPodAnnotation(context.Context, *nodev1.GetPodAnnotationRequest) (*nodev1.GetPodAnnotationResponse, error) {
	a.calls.Add(1)
	return &nodev1.GetPodAnnotationResponse{Found: true, Value: ""}, nil
}

func runtimeDELEmptyAnnotationAgent(t *testing.T) (*runtimeDELAnnotationAgent, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cni-del-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agent := &runtimeDELAnnotationAgent{}
	nodev1.RegisterNodeAgentServer(server, agent)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(func() { server.Stop(); _ = lis.Close() })
	return agent, socket
}

func runtimeDELFixture(t *testing.T) (*libcni.CNIConfig, string, string) {
	t.Helper()
	plugin := os.Getenv("CICE_HOST_LOCAL_TEST_BINARY")
	if plugin == "" {
		t.Skip("requires the upstream host-local integration fixture binary")
	}
	if _, err := os.Stat(plugin); err != nil {
		t.Fatal(err)
	}
	cacheDir, dataDir := t.TempDir(), t.TempDir()
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executor := &runtimeDELExec{
		DefaultExec: invoke.DefaultExec{RawExec: &invoke.RawExec{Stderr: os.Stderr}, PluginDecoder: version.PluginDecoder{}},
		helper:      helper, cacheDir: cacheDir,
	}
	return libcni.NewCNIConfigWithCacheDir([]string{filepath.Dir(plugin)}, cacheDir, executor), cacheDir, dataDir
}

func runtimeDELList(t *testing.T, dataDir, delegateName string, preserve bool) *libcni.NetworkConfigList {
	t.Helper()
	delegate := map[string]interface{}{
		"type": "host-local",
		"ipam": map[string]interface{}{"type": "host-local", "dataDir": dataDir, "ranges": []interface{}{[]interface{}{map[string]interface{}{"subnet": "10.244.1.0/24"}}}},
	}
	if delegateName != "" {
		delegate["name"] = delegateName
	}
	gate := map[string]interface{}{"type": "cni-gate", "delegate": delegate, "agentSocket": "/test/current-agent.sock"}
	if preserve {
		gate["preserveDelegateName"] = true
	}
	data, err := json.Marshal(map[string]interface{}{"name": "cybericebox", "cniVersion": "0.3.1", "plugins": []interface{}{gate}})
	if err != nil {
		t.Fatal(err)
	}
	list, err := libcni.ConfListFromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

type runtimeDELExec struct {
	invoke.DefaultExec
	helper, cacheDir string
}

func (e *runtimeDELExec) FindInPath(plugin string, paths []string) (string, error) {
	if plugin == "cni-gate" {
		return e.helper, nil
	}
	return e.DefaultExec.FindInPath(plugin, paths)
}

func (e *runtimeDELExec) ExecPlugin(ctx context.Context, plugin string, input []byte, environ []string) ([]byte, error) {
	if plugin != e.helper {
		return e.DefaultExec.ExecPlugin(ctx, plugin, input, environ)
	}
	cmd := exec.CommandContext(ctx, e.helper, "-test.run=^TestRuntimeDELPluginProcess$")
	cmd.Env = append(environ, "CICE_RUNTIME_DEL_PROCESS=1", "CICE_RUNTIME_DEL_CACHE_DIR="+e.cacheDir)
	cmd.Stdin = strings.NewReader(string(input))
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return output, fmt.Errorf("gate callback: %w: %s", err, output)
	}
	return output, nil
}

func TestRuntimeDELPluginProcess(t *testing.T) {
	if os.Getenv("CICE_RUNTIME_DEL_PROCESS") != "1" {
		return
	}
	libcni.CacheDir = os.Getenv("CICE_RUNTIME_DEL_CACHE_DIR")
	if os.Getenv("CNI_COMMAND") == "DEL" {
		path := filepath.Join(libcni.CacheDir, "results", "cybericebox-"+os.Getenv("CNI_CONTAINERID")+"-"+os.Getenv("CNI_IFNAME"))
		_, err := os.Stat(path)
		data, _ := json.Marshal(map[string]bool{"present": err == nil})
		if err := os.WriteFile(filepath.Join(libcni.CacheDir, "del-callback.json"), data, 0o600); err != nil {
			os.Exit(2)
		}
	}
	Run()
	os.Exit(0)
}
