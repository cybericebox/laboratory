package microvm

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The fragment is read by build.sh, which checks every line against the built .config: a line must be CONFIG_X=y, once, and the
// features the platform promises to authors must be there.
func TestFragmentIsWellFormedAndPromisesTheModuleList(t *testing.T) {
	f, err := os.Open("cybericebox.fragment")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	line := regexp.MustCompile(`^CONFIG_[A-Z0-9_]+=y$`)
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if !line.MatchString(l) {
			t.Errorf("not a built-in option line: %q", l)
		}
		if seen[l] {
			t.Errorf("duplicate: %q", l)
		}
		seen[l] = true
	}
	for _, must := range []string{"CONFIG_NF_TABLES=y", "CONFIG_BRIDGE=y", "CONFIG_VLAN_8021Q=y", "CONFIG_WIREGUARD=y", "CONFIG_TUN=y", "CONFIG_BONDING=y", "CONFIG_NF_CONNTRACK_FTP=y", "CONFIG_NF_CONNTRACK=y"} {
		if !seen[must] {
			t.Errorf("the promised %s is missing", must)
		}
	}
}
