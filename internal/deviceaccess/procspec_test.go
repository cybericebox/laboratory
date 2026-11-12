package deviceaccess

import (
	"reflect"
	"strings"
	"testing"
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

func TestProcessForNeverWidensCapabilities(t *testing.T) {
	base := deviceSpec().Process
	for _, a := range []ExecArgs{
		{Args: []string{"x"}},
		{Args: []string{"x"}, Terminal: true},
		{Args: []string{"x"}, User: &ExecUser{UID: 0, GID: 0, Groups: []uint32{4}, Home: "/root"}},
	} {
		p, err := ProcessFor(base, a)
		if err != nil {
			t.Fatal(err)
		}
		c, b := p.Capabilities, base.Capabilities
		for _, pair := range [][2][]string{{c.Bounding, b.Bounding}, {c.Effective, b.Effective}, {c.Permitted, b.Permitted}, {c.Inheritable, b.Inheritable}, {c.Ambient, b.Ambient}} {
			have := map[string]bool{}
			for _, x := range pair[1] {
				have[x] = true
			}
			for _, x := range pair[0] {
				if !have[x] {
					t.Fatalf("capability %s is wider than the container's", x)
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
