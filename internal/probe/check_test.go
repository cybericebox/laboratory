//go:build unix

package probe

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCheckExecOutcomes(t *testing.T) {
	for _, c := range []struct {
		name string
		req  ExecRequest
		want func(ExecResponse) bool
	}{
		{"exit 0", ExecRequest{Argv: []string{"true"}, TimeoutMs: 5000}, func(r ExecResponse) bool { return r.Started && r.Exit == 0 && !r.TimedOut }},
		{"exit 1", ExecRequest{Argv: []string{"false"}, TimeoutMs: 5000}, func(r ExecResponse) bool { return r.Started && r.Exit == 1 }},
		{"exit 3 with output on both streams", ExecRequest{Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, TimeoutMs: 5000},
			func(r ExecResponse) bool {
				return r.Started && r.Exit == 3 && strings.Contains(string(r.Output), "out") && strings.Contains(string(r.Output), "err")
			}},
		{"no such command", ExecRequest{Argv: []string{"definitely-not-a-command-xyz"}, TimeoutMs: 5000},
			func(r ExecResponse) bool { return !r.Started && r.Error == "cannot start: no such command" }},
		{"empty command", ExecRequest{TimeoutMs: 5000}, func(r ExecResponse) bool { return !r.Started && r.Error != "" }},
		{"killed by a signal", ExecRequest{Argv: []string{"sh", "-c", "kill -9 $$"}, TimeoutMs: 5000},
			func(r ExecResponse) bool { return r.Started && r.Exit == -1 && r.Signal != "" && !r.TimedOut }},
		{"unknown user", ExecRequest{Argv: []string{"true"}, User: "no-such-user-xyz", TimeoutMs: 5000},
			func(r ExecResponse) bool { return !r.Started && r.Error == "no such user" }},
		{"script through the interpreter", ExecRequest{Script: "echo from script; exit 4", Interpreter: "/bin/sh", TimeoutMs: 5000},
			func(r ExecResponse) bool {
				return r.Started && r.Exit == 4 && strings.Contains(string(r.Output), "from script")
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if r := runCheckExec(c.req); !c.want(r) {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestCheckExecTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survivor")
	// The shell starts a child that would create the marker after the limit; both must die with the limit.
	script := "(sleep 1; touch " + marker + ") & sleep 30"
	r := runCheckExec(ExecRequest{Argv: []string{"sh", "-c", script}, TimeoutMs: 200})
	if !r.Started || !r.TimedOut {
		t.Fatalf("got %+v", r)
	}
	// Give a surviving child the time it would need.
	waitFor(t, 1500, func() bool { _, err := os.Stat(marker); return err == nil }, false)
}

func TestCheckExecOutputIsCapped(t *testing.T) {
	r := runCheckExec(ExecRequest{Argv: []string{"sh", "-c", "head -c 300000 /dev/zero | tr '\\0' a"}, TimeoutMs: 10000})
	if !r.Started || r.Exit != 0 || len(r.Output) != MaxCheckOutput || !r.Truncated {
		t.Fatalf("started %v exit %d got %d bytes truncated %v", r.Started, r.Exit, len(r.Output), r.Truncated)
	}
}

func TestCheckFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	r := runCheckFile(FileRequest{Path: path, WantContent: true})
	if !r.Exists || !r.Regular || r.Mode != 0o640 || string(r.Content) != "hello" || r.UID != uint32(os.Getuid()) {
		t.Fatalf("got %+v", r)
	}
	if r = runCheckFile(FileRequest{Path: path}); r.Content != nil {
		t.Fatalf("content was not asked for: %+v", r)
	}
	if r = runCheckFile(FileRequest{Path: filepath.Join(dir, "nope")}); r.Exists || r.Error != "" {
		t.Fatalf("missing: %+v", r)
	}
	if r = runCheckFile(FileRequest{Path: dir, WantContent: true}); !r.Exists || r.Regular || r.Content != nil {
		t.Fatalf("directory: %+v", r)
	}
	if r = runCheckFile(FileRequest{Path: "relative"}); r.Error == "" {
		t.Fatalf("relative path: %+v", r)
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", MaxCheckFile+10)), 0o644); err != nil {
		t.Fatal(err)
	}
	if r = runCheckFile(FileRequest{Path: big, WantContent: true}); len(r.Content) != MaxCheckFile || !r.Truncated {
		t.Fatalf("big: %d bytes truncated %v", len(r.Content), r.Truncated)
	}
}

func TestParseIDNames(t *testing.T) {
	n := ParseIDNames([]byte("root:x:0:0::/root:/bin/sh\n# c\nweb:x:1000:1001::/:/bin/sh\nroot2:x:0:0::/:/bin/sh\nbad\n"), []byte("root:x:0:\nweb:x:1001:web\n"))
	if n.Users[0] != "root" || n.Users[1000] != "web" || n.Groups[1001] != "web" || len(n.Users) != 2 {
		t.Fatalf("got %+v", n)
	}
}

func TestRunCheckCommandUsage(t *testing.T) {
	if RunCheckCommand(CommandCheckExec, nil) != 2 || RunCheckCommand(CommandCheckFile, []string{"{"}) != 2 {
		t.Fatal("bad usage must exit 2")
	}
}

// waitFor polls cond for up to ms milliseconds; want says whether it has to become true (false: it must stay false).
func waitFor(t *testing.T, ms int, cond func() bool, want bool) {
	t.Helper()
	for i := 0; i < ms/50; i++ {
		if cond() {
			if !want {
				t.Fatal("a process of the killed group survived")
			}
			return
		}
		sleep50()
	}
	if want {
		t.Fatal("timed out")
	}
}

func sleep50() { time.Sleep(50 * time.Millisecond) }

func TestResolveUser(t *testing.T) {
	etc := t.TempDir()
	if err := os.WriteFile(filepath.Join(etc, "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\nweb:x:1000:1001::/srv/web:/bin/sh\nnobody:x:65534:65533::/nonexistent:/bin/false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in   string
		want account
		err  string
	}{
		{"root", account{0, 0, "root", "/root"}, ""},
		{"web", account{1000, 1001, "web", "/srv/web"}, ""},
		{"1000", account{1000, 1001, "web", "/srv/web"}, ""},
		{"1000:50", account{1000, 50, "web", "/srv/web"}, ""},
		{"4242", account{4242, 4242, "", "/"}, ""},
		{"nobody", account{65534, 65533, "nobody", "/nonexistent"}, ""},
		{"ghost", account{}, "no such user"},
		{"web:x", account{}, "bad group"},
	} {
		got, err := resolveUser(etc, c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: %v", c.in, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %+v %v, want %+v", c.in, got, err, c.want)
		}
	}
	// An image with no passwd file at all still has nobody.
	if got, err := resolveUser(t.TempDir(), "nobody"); err != nil || got.uid != 65534 || got.gid != 65534 {
		t.Errorf("nobody without a passwd file: %+v %v", got, err)
	}
}

func TestCleanEnvironment(t *testing.T) {
	t.Setenv("SECRET_FROM_THE_IMAGE", "flag")
	t.Setenv("PATH", "/image/bin")
	r := runCheckExec(ExecRequest{Argv: []string{"env"}, TimeoutMs: 5000})
	out := string(r.Output)
	if !r.Started || strings.Contains(out, "SECRET_FROM_THE_IMAGE") || strings.Contains(out, "/image/bin") || !strings.Contains(out, "PATH="+checkPath) {
		t.Fatalf("got %+v", r)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "PATH=") && !strings.HasPrefix(line, "HOME=") && !strings.HasPrefix(line, "LC_ALL=") && !strings.HasPrefix(line, "USER=") && !strings.HasPrefix(line, "LOGNAME=") && !strings.HasPrefix(line, "PWD=") && !strings.HasPrefix(line, "SHLVL=") && !strings.HasPrefix(line, "_=") {
			t.Errorf("an unexpected variable: %q", line)
		}
	}
}

// A check for nobody: when the tool is not root it cannot switch, and says so rather than run as somebody else.
func TestNobodyNeedsARootTool(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can switch")
	}
	r := runCheckExec(ExecRequest{Argv: []string{"true"}, User: "nobody", TimeoutMs: 5000})
	if r.Started || !strings.Contains(r.Error, "does not run as root") {
		t.Fatalf("got %+v", r)
	}
	// Asking for the user the tool runs as is fine.
	me := strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid())
	if r := runCheckExec(ExecRequest{Argv: []string{"true"}, User: me, TimeoutMs: 5000}); !r.Started || r.Exit != 0 {
		t.Fatalf("own user: %+v", r)
	}
}
