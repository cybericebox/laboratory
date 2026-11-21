package deviceaccess

import (
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
)

func TestProcessForCopiesTheContainer(t *testing.T) {
	base := deviceSpec().Process
	p, err := ProcessFor(base, ExecArgs{Args: []string{"/bin/sh"}, Terminal: true, Cols: 90, Rows: 20})
	if err != nil {
		t.Fatal(err)
	}
	// An empty set may come back as an absent one (the spec omits it); the two mean the same.
	c, b := p.Capabilities, base.Capabilities
	for i, pair := range [][2][]string{{c.Bounding, b.Bounding}, {c.Effective, b.Effective}, {c.Permitted, b.Permitted}, {c.Inheritable, b.Inheritable}, {c.Ambient, b.Ambient}} {
		if len(pair[0]) != len(pair[1]) || len(pair[0]) > 0 && !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("capability set %d differs from the container's: %v vs %v", i, pair[0], pair[1])
		}
	}
	if !p.NoNewPrivileges || p.User.UID != 1000 || p.Cwd != "/home/x" {
		t.Fatalf("restrictions of the container were lost: %+v", p)
	}
	if !p.Terminal || p.ConsoleSize == nil || p.ConsoleSize.Width != 90 || p.ConsoleSize.Height != 20 {
		t.Fatalf("terminal: %+v", p)
	}
	if !reflect.DeepEqual(p.Args, []string{"/bin/sh"}) || p.CommandLine != "" {
		t.Fatalf("args: %+v", p.Args)
	}
	want := map[string]bool{"PATH=/usr/bin": true, "FOO=bar": true, "TERM=xterm-256color": true}
	for _, e := range p.Env {
		delete(want, e)
	}
	if len(want) != 0 {
		t.Fatalf("env %v misses %v", p.Env, want)
	}
}

// Whatever the user, no set holds a capability outside the container's bounding set.
func TestProcessForNeverWidensCapabilities(t *testing.T) {
	base := deviceSpec().Process
	bound := map[string]bool{}
	for _, x := range base.Capabilities.Bounding {
		bound[x] = true
	}
	for _, a := range []ExecArgs{
		{Args: []string{"x"}},
		{Args: []string{"x"}, Terminal: true},
		{Args: []string{"x"}, User: &ExecUser{UID: 0, GID: 0, Groups: []uint32{4}, Home: "/root"}},
		{Args: []string{"x"}, User: &ExecUser{UID: 1000, GID: 1000}},
	} {
		p, err := ProcessFor(base, a)
		if err != nil {
			t.Fatal(err)
		}
		c := p.Capabilities
		for _, set := range [][]string{c.Bounding, c.Effective, c.Permitted, c.Inheritable, c.Ambient} {
			for _, x := range set {
				if !bound[x] {
					t.Fatalf("capability %s is outside the container's bounding set", x)
				}
			}
		}
	}
}

func TestProcessForDoesNotChangeTheContainerSpec(t *testing.T) {
	base := deviceSpec().Process
	before := *deviceSpec().Process
	p, _ := ProcessFor(base, ExecArgs{Args: []string{"x"}, Terminal: true, User: &ExecUser{UID: 5}})
	p.Env[0] = "CHANGED=1"
	p.Capabilities.Bounding[0] = "CAP_SYS_ADMIN"
	if !reflect.DeepEqual(*base, before) {
		t.Fatalf("the container's spec was modified: %+v", base)
	}
}

func TestProcessForUser(t *testing.T) {
	p, _ := ProcessFor(deviceSpec().Process, ExecArgs{Args: []string{"x"}, User: &ExecUser{UID: 0, GID: 0, Groups: []uint32{27}, Home: "/root"}})
	if p.User.UID != 0 || p.User.GID != 0 || len(p.User.AdditionalGids) != 1 {
		t.Fatalf("user: %+v", p.User)
	}
	found := false
	for _, e := range p.Env {
		found = found || e == "HOME=/root"
	}
	if !found {
		t.Fatalf("env: %v", p.Env)
	}
}

func TestCheckSettings(t *testing.T) {
	if c, err := CheckSettings(nil, ""); err != nil || c[0] != "/bin/sh" {
		t.Fatalf("default: %v %v", c, err)
	}
	if _, err := CheckSettings([]string{"/bin/bash", "-l"}, "kali"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		cmd  []string
		user string
	}{
		"too many items":    {cmd: make([]string, 33)},
		"long item":         {cmd: []string{strings.Repeat("a", 5000)}},
		"NUL":               {cmd: []string{"a\x00b"}},
		"empty program":     {cmd: []string{""}},
		"user with a space": {user: "a b"},
		"user with a slash": {user: "../root"},
		"user injection":    {user: "root;id"},
	} {
		if len(c.cmd) == 33 {
			for i := range c.cmd {
				c.cmd[i] = "x"
			}
		}
		if _, err := CheckSettings(c.cmd, c.user); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	for _, u := range []string{"root", "0", "1000:1000", "kali:users", "_apt", "www-data"} {
		if _, err := CheckSettings(nil, u); err != nil {
			t.Fatalf("user %q refused: %v", u, err)
		}
	}
}

func TestProcessForNonRootUserKeepsOnlyTheBoundingSet(t *testing.T) {
	base := deviceSpec().Process
	p, err := ProcessFor(base, ExecArgs{Args: []string{"x"}, User: &ExecUser{UID: 1000, GID: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Capabilities
	if len(c.Effective) != 0 || len(c.Permitted) != 0 || len(c.Ambient) != 0 || len(c.Inheritable) != 0 {
		t.Fatalf("an ordinary user got capabilities: %+v", c)
	}
	if !reflect.DeepEqual(c.Bounding, base.Capabilities.Bounding) {
		t.Fatalf("the bounding set changed: %v", c.Bounding)
	}
	// the container's own sets are untouched
	if len(base.Capabilities.Effective) == 0 {
		t.Fatal("the container's spec was modified")
	}
}

// A root exec in a container that itself runs as an ordinary user: its sets are empty, the bounding set holds the device's capabilities. A
// root with empty sets could not drop to another user or read a file of another user, so effective and permitted become the bounding set.
func TestProcessForRootInANonRootImageGetsTheBoundingSet(t *testing.T) {
	base := deviceSpec().Process // runs as uid 1000
	base.Capabilities.Effective, base.Capabilities.Permitted = nil, nil
	base.Capabilities.Bounding = []string{"CAP_SETUID", "CAP_SETGID", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_CHOWN"}
	base.Capabilities.Inheritable, base.Capabilities.Ambient = []string{"CAP_CHOWN"}, []string{"CAP_CHOWN"}
	p, err := ProcessFor(base, ExecArgs{Args: []string{"x"}, User: &ExecUser{UID: 0, GID: 0}})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Capabilities
	if !reflect.DeepEqual(c.Effective, base.Capabilities.Bounding) || !reflect.DeepEqual(c.Permitted, base.Capabilities.Bounding) || !reflect.DeepEqual(c.Bounding, base.Capabilities.Bounding) {
		t.Fatalf("effective %v permitted %v bounding %v", c.Effective, c.Permitted, c.Bounding)
	}
	if len(c.Inheritable) != 0 || len(c.Ambient) != 0 {
		t.Fatalf("inheritable %v ambient %v must stay empty", c.Inheritable, c.Ambient)
	}
	// the container's own spec is not touched
	if len(base.Capabilities.Effective) != 0 {
		t.Fatal("the container's spec was modified")
	}
	// the slices are copies: changing the result must not change the bounding set
	c.Effective[0] = "CAP_SYS_ADMIN"
	if base.Capabilities.Bounding[0] != "CAP_SETUID" || p.Capabilities.Bounding[0] != "CAP_SETUID" || c.Permitted[0] != "CAP_SETUID" {
		t.Fatal("the sets share memory")
	}
}

// A profile with a reduced bounding set gives a reduced root: nothing outside the bounding set, whatever the device's other sets say.
func TestProcessForRootNeverGetsMoreThanTheBoundingSet(t *testing.T) {
	for _, imageUID := range []uint32{0, 1000} {
		base := deviceSpec().Process
		base.User.UID = imageUID
		base.Capabilities.Bounding = []string{"CAP_NET_BIND_SERVICE"}
		base.Capabilities.Effective = []string{"CAP_NET_BIND_SERVICE"}
		base.Capabilities.Permitted = []string{"CAP_NET_BIND_SERVICE"}
		p, _ := ProcessFor(base, ExecArgs{Args: []string{"x"}, User: &ExecUser{UID: 0}})
		for _, set := range [][]string{p.Capabilities.Effective, p.Capabilities.Permitted, p.Capabilities.Bounding, p.Capabilities.Inheritable, p.Capabilities.Ambient} {
			for _, x := range set {
				if x != "CAP_NET_BIND_SERVICE" {
					t.Fatalf("image uid %d: %s is outside the reduced bounding set", imageUID, x)
				}
			}
		}
		if len(p.Capabilities.Effective) != 1 || len(p.Capabilities.Permitted) != 1 {
			t.Fatalf("image uid %d: effective %v permitted %v", imageUID, p.Capabilities.Effective, p.Capabilities.Permitted)
		}
	}
}

// A root exec in a container that runs as root is the container's own sets, unchanged.
func TestProcessForRootInARootImageIsUnchanged(t *testing.T) {
	base := deviceSpec().Process
	base.User.UID = 0
	base.Capabilities.Effective = []string{"CAP_CHOWN"} // narrower than the bounding set on purpose
	base.Capabilities.Permitted = []string{"CAP_CHOWN"}
	for _, u := range []*ExecUser{nil, {UID: 0}} {
		p, _ := ProcessFor(base, ExecArgs{Args: []string{"x"}, User: u})
		if !reflect.DeepEqual(p.Capabilities.Effective, []string{"CAP_CHOWN"}) || !reflect.DeepEqual(p.Capabilities.Permitted, []string{"CAP_CHOWN"}) ||
			!reflect.DeepEqual(p.Capabilities.Bounding, base.Capabilities.Bounding) {
			t.Fatalf("user %v: %+v", u, p.Capabilities)
		}
	}
}

func TestProcessForCarriesTheRestOfTheContainersRestrictions(t *testing.T) {
	base := deviceSpec().Process
	score := 500
	base.ApparmorProfile, base.SelinuxLabel, base.OOMScoreAdj = "cri-containerd.apparmor.d", "system_u:system_r:container_t:s0", &score
	base.Rlimits = []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: 1024, Soft: 1024}}
	for _, u := range []*ExecUser{nil, {UID: 0}, {UID: 1000}} {
		p, err := ProcessFor(base, ExecArgs{Args: []string{"x"}, Terminal: true, User: u})
		if err != nil {
			t.Fatal(err)
		}
		if !p.NoNewPrivileges || p.ApparmorProfile != base.ApparmorProfile || p.SelinuxLabel != base.SelinuxLabel || p.OOMScoreAdj == nil || *p.OOMScoreAdj != 500 ||
			len(p.Rlimits) != 1 || p.Rlimits[0].Hard != 1024 {
			t.Fatalf("user %v: a restriction of the container was lost: %+v", u, p)
		}
	}
}

func TestCheckSettingsCommandRules(t *testing.T) {
	for _, bad := range [][]string{{"sh"}, {"bin/sh"}, {"/proc/self/exe"}, {"/proc"}, {"/sys/kernel/x"}, {"/proc/../proc/1/exe"}, {"/./sys/x"}, {"/bin/../proc/self/exe"}, {"./sh"}} {
		if _, err := CheckSettings(bad, ""); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	for _, ok := range [][]string{{"/bin/sh"}, {"/usr/bin/env", "bash"}, {"/procfoo/x"}, {"/opt/sysmon/run"}} {
		if _, err := CheckSettings(ok, ""); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	if CommandHash([]string{"/bin/sh", "-l"}) == CommandHash([]string{"/bin/sh"}) || len(CommandHash(nil)) != 12 {
		t.Error("command hash")
	}
}
