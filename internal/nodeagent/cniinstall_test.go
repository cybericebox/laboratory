package nodeagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fastCNIRetry(t *testing.T) {
	t.Helper()
	old := cniRetryInterval
	cniRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { cniRetryInterval = old })
}

func TestInstallCNIConfWaitsForTheRealConfigAndWritesNoFallback(t *testing.T) {
	fastCNIRetry(t)
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- InstallCNIConf(dir, "/run/sock", 0) }()

	select {
	case err := <-done:
		t.Fatalf("returned with no base config: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(dir, CNIConfFile)); err == nil {
		t.Fatal("a conflist was written although no base CNI config exists")
	}

	base := `{"cniVersion":"1.0.0","name":"cilium","plugins":[{"type":"cilium-cni"}]}`
	if err := os.WriteFile(filepath.Join(dir, "05-cilium.conflist"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not pick the base config up")
	}
	data, err := os.ReadFile(filepath.Join(dir, CNIConfFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !contains(got, "cilium-cni") || contains(got, "host-local") {
		t.Errorf("the conflist must delegate to the real CNI:\n%s", got)
	}
}

func TestInstallCNIConfFallbackOnlyOnOptIn(t *testing.T) {
	fastCNIRetry(t)
	dir := t.TempDir()
	if err := InstallCNIConf(dir, "/run/sock", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, CNIConfFile))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), "host-local") {
		t.Errorf("the opt-in fallback must be written:\n%s", data)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestWriteCNIConfPreservesBaseNetworkIdentityWithoutMutatingBase(t *testing.T) {
	for _, tc := range []struct {
		name string
		base string
	}{
		{
			name: "conflist",
			base: `{"cniVersion":"0.3.1","name":"kindnet","plugins":[{"type":"ptp","name":"plugin-name-is-not-the-network","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}},{"type":"portmap","capabilities":{"portMappings":true}}]}`,
		},
		{
			name: "plain-conf",
			base: `{"cniVersion":"0.3.1","name":"kindnet","type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var base map[string]interface{}
			if err := json.Unmarshal([]byte(tc.base), &base); err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := writeCNIConf(dir, "/run/sock", base); err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("base config mutated: before=%s after=%s", before, after)
			}
			data, err := os.ReadFile(filepath.Join(dir, CNIConfFile))
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Name       string `json:"name"`
				CNIVersion string `json:"cniVersion"`
				Plugins    []struct {
					Type        string                 `json:"type"`
					AgentSocket string                 `json:"agentSocket"`
					Delegate    map[string]interface{} `json:"delegate"`
				} `json:"plugins"`
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Name != "cybericebox" || got.CNIVersion != "0.3.1" || len(got.Plugins) != 1 {
				t.Fatalf("wrapper identity/first-plugin behavior changed: %s", data)
			}
			plugin := got.Plugins[0]
			if plugin.Type != "cni-gate" || plugin.AgentSocket != "/run/sock" {
				t.Fatalf("wrapper settings changed: %+v", plugin)
			}
			var want map[string]interface{}
			wantJSON := `{"name":"kindnet","type":"ptp","mtu":1500,"ipMasq":false,"ipam":{"type":"host-local","dataDir":"/run/cni-ipam-state","ranges":[[{"subnet":"10.244.1.0/24"}]],"routes":[{"dst":"0.0.0.0/0"}]}}`
			if tc.name == "plain-conf" {
				wantJSON = tc.base
			}
			if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plugin.Delegate, want) {
				t.Errorf("delegate must retain base network identity and fields: got=%v want=%v", plugin.Delegate, want)
			}
		})
	}
}
