package cnigate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/skel"

	"github.com/cybericebox/laboratory/internal/nodeagent"
)

// Exercise the installer, libcni's wrapper-name injection and the real ADD/DEL
// callers. Only the external plugin is replaced; its stdin is the contract that
// decides the host-local allocation domain.
func TestInstalledDelegateIdentityForADDAndDEL(t *testing.T) {
	for _, tc := range []struct {
		name             string
		base             string
		cached           string
		wantName         string
		wantIPAM         string
		wantPreserveName bool
	}{
		{
			name:             "kindnet-conflist",
			base:             `{"cniVersion":"0.3.1","name":"kindnet","plugins":[{"type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}},{"type":"portmap"}]}`,
			wantName:         "kindnet",
			wantIPAM:         `{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}`,
			wantPreserveName: true,
		},
		{
			name:             "plain-conf",
			base:             `{"cniVersion":"0.3.1","name":"kindnet","type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}}`,
			wantName:         "kindnet",
			wantIPAM:         `{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}`,
			wantPreserveName: true,
		},
		{
			name:     "cached-legacy-unnamed-delegate",
			cached:   `{"cniVersion":"0.3.1","name":"cybericebox","plugins":[{"type":"cni-gate","agentSocket":"/run/sock","delegate":{"type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}}}]}`,
			wantName: "cybericebox",
			wantIPAM: `{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}`,
		},
		{
			name:     "cached-legacy-named-plain-delegate",
			cached:   `{"cniVersion":"0.3.1","name":"cybericebox","plugins":[{"type":"cni-gate","agentSocket":"/run/sock","delegate":{"cniVersion":"0.3.1","name":"kindnet","type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}}}]}`,
			wantName: "cybericebox",
			wantIPAM: `{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}`,
		},
		{
			name:     "cached-legacy-named-first-plugin",
			cached:   `{"cniVersion":"0.3.1","name":"cybericebox","plugins":[{"type":"cni-gate","agentSocket":"/run/sock","delegate":{"name":"first-plugin-name","type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}}}]}`,
			wantName: "cybericebox",
			wantIPAM: `{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}`,
		},
		{
			name:     "explicit-fallback",
			wantName: "cybericebox",
			wantIPAM: `{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.0.0/16"}]],"routes":[{"dst":"0.0.0.0/0"}]}`,
		},
		{
			name:     "missing-base-name",
			base:     `{"cniVersion":"0.3.1","plugins":[{"type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local"}}]}`,
			wantName: "cybericebox",
			wantIPAM: `{"type":"host-local"}`,
		},
		{
			name:     "missing-base-name-with-named-plugin",
			base:     `{"cniVersion":"0.3.1","plugins":[{"name":"first-plugin-name","type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local"}}]}`,
			wantName: "cybericebox",
			wantIPAM: `{"type":"host-local"}`,
		},
		{
			name:     "empty-base-name",
			base:     `{"cniVersion":"0.3.1","name":"","type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local"}}`,
			wantName: "cybericebox",
			wantIPAM: `{"type":"host-local"}`,
		},
		{
			name:     "invalid-base-name-type",
			base:     `{"cniVersion":"0.3.1","name":42,"type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local"}}`,
			wantName: "cybericebox",
			wantIPAM: `{"type":"host-local"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			baseFile := filepath.Join(dir, "10-base.conf")
			if tc.base != "" {
				if err := os.WriteFile(baseFile, []byte(tc.base), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			data := []byte(tc.cached)
			if tc.cached == "" {
				if err := nodeagent.InstallCNIConf(dir, "/run/sock", time.Nanosecond); err != nil {
					t.Fatal(err)
				}
				var err error
				data, err = os.ReadFile(filepath.Join(dir, nodeagent.CNIConfFile))
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.base != "" {
				unchanged, err := os.ReadFile(baseFile)
				if err != nil || string(unchanged) != tc.base {
					t.Fatalf("base CNI file changed: error=%v data=%s", err, unchanged)
				}
			}
			list, err := libcni.ConfListFromBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			if list.Name != "cybericebox" || list.CNIVersion != "0.3.1" || len(list.Plugins) != 1 {
				t.Fatalf("wrapper identity/first-plugin behavior changed: %+v", list)
			}
			plugin, err := libcni.InjectConf(list.Plugins[0], map[string]interface{}{
				"name": list.Name, "cniVersion": list.CNIVersion,
			})
			if err != nil {
				t.Fatal(err)
			}
			var gate map[string]interface{}
			if err := json.Unmarshal(plugin.Bytes, &gate); err != nil {
				t.Fatal(err)
			}
			if preserve, present := gate["preserveDelegateName"]; tc.wantPreserveName {
				if preserve != true {
					t.Errorf("new valid base must mark its delegate name authoritative: %s", plugin.Bytes)
				}
			} else if present {
				t.Errorf("legacy/fallback/invalid base config must not opt into a different allocation domain: %s", plugin.Bytes)
			}
			conf, err := loadConf(plugin.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(conf.Delegate)
			if err != nil {
				t.Fatal(err)
			}
			_ = marshalDelegate(conf)
			after, err := json.Marshal(conf.Delegate)
			if err != nil || string(after) != string(before) {
				t.Fatalf("marshalling mutated stored delegate: error=%v before=%s after=%s", err, before, after)
			}
			capture := captureIdentityDelegate(t)
			args := &skel.CmdArgs{ContainerID: "identity-test", Netns: "/test/netns", IfName: "eth0", StdinData: plugin.Bytes}
			if err := cmdADD(args); err != nil {
				t.Fatal(err)
			}
			if err := cmdDEL(args); err != nil {
				t.Fatal(err)
			}
			var add map[string]interface{}
			for _, command := range []string{"ADD", "DEL"} {
				payload, err := os.ReadFile(filepath.Join(capture, command+".json"))
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]interface{}
				if err := json.Unmarshal(payload, &got); err != nil {
					t.Fatal(err)
				}
				if got["name"] != tc.wantName || got["cniVersion"] != "0.3.1" || got["type"] != "ptp" || got["mtu"] != float64(1500) {
					t.Errorf("%s delegate identity/settings changed: %s", command, payload)
				}
				var wantIPAM map[string]interface{}
				if err := json.Unmarshal([]byte(tc.wantIPAM), &wantIPAM); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got["ipam"], wantIPAM) || got["ipMasq"] != (tc.name == "explicit-fallback") {
					t.Errorf("%s delegate IPAM/settings changed: %s", command, payload)
				}
				if command == "ADD" {
					add = got
				} else if !reflect.DeepEqual(got, add) {
					t.Errorf("ADD/DEL used different delegate configs: ADD=%v DEL=%v", add, got)
				}
			}
		})
	}
}

func captureIdentityDelegate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
cat > "$CICE_IDENTITY_CAPTURE/$CNI_COMMAND.json"
if [ "$CNI_COMMAND" = ADD ]; then
  echo '{"cniVersion":"0.3.1","interfaces":[{"name":"eth0","sandbox":"/test/netns"}],"ips":[{"version":"4","interface":0,"address":"10.244.1.21/24"}]}'
fi
`
	if err := os.WriteFile(filepath.Join(dir, "ptp"), []byte(script), 0o755); err != nil { //nolint:gosec // executable capture fixture
		t.Fatal(err)
	}
	t.Setenv("CNI_PATH", dir)
	t.Setenv("CICE_IDENTITY_CAPTURE", dir)
	t.Setenv("CNI_CONTAINERID", "identity-test")
	t.Setenv("CNI_NETNS", "/test/netns")
	t.Setenv("CNI_IFNAME", "eth0")
	return dir
}
