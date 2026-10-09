package nodeagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// CNIConfDir is the standard directory kubelet scans for CNI configs.
	CNIConfDir = "/etc/cni/net.d"

	// CNIConfFile is our CNI conflist filename. The "00-" prefix guarantees
	// lexicographic priority over all other CNI configs (Multus uses "00-multus.conf",
	// but "00-cybericebox" < "00-multus" alphabetically).
	CNIConfFile = "00-cybericebox.conflist"

	// CNIBinDir is where CNI plugin binaries are installed.
	CNIBinDir = "/opt/cni/bin"

	defaultCNIRetryInterval = 5 * time.Second
)

// cniRetryInterval is how often InstallCNIConf looks for the base config again (a variable for tests).
var cniRetryInterval = defaultCNIRetryInterval

// InstallCNIConf writes CNIConfFile into confDir by wrapping the first
// existing CNI config it finds with cni-gate. Blocks until a base config
// appears (retry every 5 s): the real CNI (Cilium) can take minutes on a fresh
// node (image pull, agent start), and until it is there no pod may be given a
// network. There is no automatic fallback: only when fallbackTimeout is
// positive (an explicit opt-in, nodeAgent.cniFallbackTimeout) is a built-in
// ptp delegate written after that long without a base config.
func InstallCNIConf(confDir, agentSocket string, fallbackTimeout time.Duration) error {
	var deadline time.Time
	if fallbackTimeout > 0 {
		deadline = time.Now().Add(fallbackTimeout)
	}
	for {
		base, err := findBaseCNIConf(confDir)
		if err == nil {
			return writeCNIConf(confDir, agentSocket, base)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return writeFallbackCNIConf(confDir, agentSocket)
		}
		fmt.Fprintf(os.Stderr, "install-cni: waiting for base CNI config in %s: %v\n", confDir, err)
		time.Sleep(cniRetryInterval)
	}
}

// findBaseCNIConf scans confDir for the first CNI config that is not ours,
// sorted lexicographically (so the currently-active CNI is picked up).
func findBaseCNIConf(confDir string) (map[string]interface{}, error) {
	entries, err := os.ReadDir(confDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", confDir, err)
	}
	sort.Slice(
		entries, func(i, j int) bool {
			return entries[i].Name() < entries[j].Name()
		},
	)
	for _, e := range entries {
		name := e.Name()
		if name == CNIConfFile {
			continue
		}
		if !strings.HasSuffix(name, ".conflist") && !strings.HasSuffix(name, ".conf") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(confDir, name))
		if err != nil {
			continue
		}
		var conf map[string]interface{}
		if err := json.Unmarshal(data, &conf); err != nil {
			continue
		}
		return conf, nil
	}
	return nil, fmt.Errorf("no base CNI config found")
}

func writeCNIConf(confDir, agentSocket string, base map[string]interface{}) error {
	delegate := extractFirstPlugin(base)
	preserveDelegateName := false
	if plugin, ok := delegate.(map[string]interface{}); ok {
		if name, ok := base["name"].(string); ok && name != "" {
			// The base network name is also the host-local allocation domain.
			// Copy the plugin so wrapping a conflist does not mutate its source.
			named := make(map[string]interface{}, len(plugin)+1)
			for k, v := range plugin {
				named[k] = v
			}
			named["name"] = name
			delegate = named
			preserveDelegateName = true
		}
	}
	return writeConf(confDir, agentSocket, base["cniVersion"], delegate, preserveDelegateName)
}

// writeFallbackCNIConf writes a self-contained conflist using ptp+host-local
// when no base CNI is available (e.g. fresh node before any other CNI is installed).
func writeFallbackCNIConf(confDir, agentSocket string) error {
	delegate := map[string]interface{}{
		"type":   "ptp",
		"ipMasq": true,
		"mtu":    1500,
		"ipam": map[string]interface{}{
			"type":    "host-local",
			"dataDir": "/run/cni-ipam-state",
			"ranges": []interface{}{
				[]interface{}{map[string]interface{}{"subnet": "10.244.0.0/16"}},
			},
			"routes": []interface{}{map[string]interface{}{"dst": "0.0.0.0/0"}},
		},
	}
	return writeConf(confDir, agentSocket, "0.3.1", delegate, false)
}

func writeConf(confDir, agentSocket string, cniVersion, delegate interface{}, preserveDelegateName bool) error {
	gate := map[string]interface{}{
		"type":        "cni-gate",
		"agentSocket": agentSocket,
		"delegate":    delegate,
	}
	if preserveDelegateName {
		gate["preserveDelegateName"] = true
	}
	conf := map[string]interface{}{
		"cniVersion": cniVersion,
		"name":       "cybericebox",
		"plugins":    []interface{}{gate},
	}
	data, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal CNI conf: %w", err)
	}
	// Write to a temp file and rename: kubelet rescans confDir continuously and
	// must never observe a half-written conflist. Rename within the same dir is
	// atomic; the ".tmp" suffix keeps kubelet from picking the temp file up
	// (it only reads .conf/.conflist/.json).
	dst := filepath.Join(confDir, CNIConfFile)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmp, dst, err)
	}
	return nil
}

// extractFirstPlugin returns the first plugin entry from a conflist, or the
// entire config when the file is a plain .conf (no "plugins" array).
func extractFirstPlugin(conf map[string]interface{}) interface{} {
	if plugins, ok := conf["plugins"].([]interface{}); ok && len(plugins) > 0 {
		return plugins[0]
	}
	return conf
}
