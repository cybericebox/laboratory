package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/labchecks"
)

// fakeSys records what the launcher does to the machine; scripts really run (with /bin/sh of the host).
type fakeSys struct {
	vars    []string
	chowns  map[string][2]int
	dropped *Identity
	exec    struct {
		path string
		argv []string
		env  []string
	}
	execErr error
	slept   []time.Duration
	paused  bool
	real    OS
}

func newFake() *fakeSys {
	return &fakeSys{vars: []string{"PATH=/bin:/usr/bin", "HOME=/root", "FROM_IMAGE=1"}, chowns: map[string][2]int{}}
}

func (f *fakeSys) Environ() []string { return f.vars }
func (f *fakeSys) Chown(path string, uid, gid int) error {
	f.chowns[filepath.Base(path)] = [2]int{uid, gid}
	return nil
}
func (f *fakeSys) Drop(id Identity) error { f.dropped = &id; return nil }
func (f *fakeSys) Exec(path string, argv, env []string) error {
	f.exec.path, f.exec.argv, f.exec.env = path, argv, env
	return f.execErr
}
func (f *fakeSys) RunScript(ctx context.Context, interpreter, script string, env []string) error {
	return f.real.RunScript(ctx, interpreter, script, env)
}
func (f *fakeSys) Sleep(d time.Duration) { f.slept = append(f.slept, d) }
func (f *fakeSys) Pause()                { f.paused = true }

type rig struct {
	t    *testing.T
	root string // the container file system
	dir  string // the launcher's directory
	log  string
	sys  *fakeSys
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "boot")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "etc/passwd"), "root:x:0:0:root:/root:/bin/sh\nweb:x:1000:1000::/home/web:/bin/sh\n")
	write(t, filepath.Join(root, "etc/group"), "root:x:0:\nweb:x:1000:\nstaff:x:50:web\nlogs:x:60:other,web\n")
	return &rig{t: t, root: root, dir: dir, log: filepath.Join(t.TempDir(), "termination-log"), sys: newFake()}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) list(b labchecks.Bootstrap) {
	r.t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		r.t.Fatal(err)
	}
	write(r.t, filepath.Join(r.dir, "bootstrap.json"), string(raw))
}

func (r *rig) config(bootstrap bool, command ...string) Config {
	return Config{Dir: r.dir, Bootstrap: bootstrap, Command: command, TerminationLog: r.log, Root: r.root}
}

func (r *rig) has(name string) bool {
	_, err := os.Lstat(filepath.Join(r.dir, name))
	return err == nil
}

func (r *rig) message() Message {
	r.t.Helper()
	b, err := os.ReadFile(r.log)
	if err != nil {
		r.t.Fatalf("no termination message: %v", err)
	}
	m, ok := ParseMessage(string(b))
	if !ok {
		r.t.Fatalf("not a launcher message: %s", b)
	}
	return m
}

func TestFilesPlacedWithOwnersAndModes(t *testing.T) {
	r := newRig(t)
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 30, Files: []labchecks.File{
		{Path: "/etc/app/app.conf", Content: []byte("key=SECRETVALUE\n"), Owner: "web", Group: "staff", Mode: 0o640},
		{Path: "/opt/run.sh", Content: []byte("#!/bin/sh\n"), Owner: "0", Group: "1000", Mode: 0o4755},
		{Path: "/etc/default.conf", Content: []byte("x")},
	}})
	if code := Run(r.config(true, "/bin/true"), r.sys); code != 0 {
		t.Fatalf("code %d, message %v", code, r.log)
	}
	check := func(path, content string, mode os.FileMode) {
		t.Helper()
		full := filepath.Join(r.root, path)
		b, err := os.ReadFile(full)
		if err != nil || string(b) != content {
			t.Fatalf("%s = %q, %v", path, b, err)
		}
		st, _ := os.Stat(full)
		if st.Mode() != mode {
			t.Fatalf("%s mode = %v, want %v", path, st.Mode(), mode)
		}
	}
	check("/etc/app/app.conf", "key=SECRETVALUE\n", 0o640)
	check("/opt/run.sh", "#!/bin/sh\n", os.ModeSetuid|0o755)
	check("/etc/default.conf", "x", 0o644)
	var owners [][2]int
	for _, v := range r.sys.chowns {
		owners = append(owners, v)
	}
	for _, want := range [][2]int{{1000, 50}, {0, 1000}, {0, 0}} {
		if !slices.Contains(owners, want) {
			t.Fatalf("owners %v lack %v", owners, want)
		}
	}
	// no temporary file stays
	entries, _ := os.ReadDir(filepath.Join(r.root, "etc/app"))
	if len(entries) != 1 {
		t.Fatalf("leftovers: %v", entries)
	}
}

func TestUnknownOwnerFailsWithoutTheName(t *testing.T) {
	r := newRig(t)
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 30, Files: []labchecks.File{{Path: "/a", Content: []byte("v"), Owner: "nobodyhere"}}})
	if code := Run(r.config(true, "/bin/true"), r.sys); code != ExitFailed {
		t.Fatalf("code %d", code)
	}
	m := r.message()
	if m.Kind != KindFailed || m.Attempt != 1 || m.Final || !strings.Contains(m.Reason, "file 1 of 1") || strings.Contains(m.Reason, "nobodyhere") {
		t.Fatalf("message %+v", m)
	}
	if !r.has("bootstrap.json") {
		t.Fatal("a failed attempt keeps the list for the next one")
	}
	if r.sys.exec.path != "" {
		t.Fatal("the service must not start after a failed bootstrap")
	}
}

func TestSuccessErasesEverythingAndStartsTheService(t *testing.T) {
	r := newRig(t)
	marker := filepath.Join(t.TempDir(), "ran")
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 30, Script: "echo SCRIPTSECRET > " + marker, Files: []labchecks.File{{Path: "/etc/v", Content: []byte("FILESECRET")}}})
	r.sys.vars = append(r.sys.vars, "DEVICE_ENV=1")
	if code := Run(r.config(true, "sh", "-c", "serve"), r.sys); code != 0 {
		t.Fatalf("code %d", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the script did not run")
	}
	if r.has("bootstrap.json") || r.has("attempts") {
		t.Fatal("the list and the counter must be gone")
	}
	for _, m := range []string{"ok", "settled", "started"} {
		if !r.has(m) {
			t.Fatalf("marker %s missing", m)
		}
	}
	// Nothing bootstrap-related is readable in the directory that stays mounted in the device.
	entries, _ := os.ReadDir(r.dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(r.dir, e.Name()))
		if len(b) != 0 {
			t.Fatalf("%s holds data: %q", e.Name(), b)
		}
	}
	if r.sys.exec.path != "/bin/sh" && r.sys.exec.path != "/usr/bin/sh" {
		t.Fatalf("exec path %q", r.sys.exec.path)
	}
	if !slices.Equal(r.sys.exec.argv, []string{"sh", "-c", "serve"}) {
		t.Fatalf("argv %v", r.sys.exec.argv)
	}
	// The service environment is the container's own, nothing added.
	if !slices.Equal(r.sys.exec.env, r.sys.vars) {
		t.Fatalf("env %v", r.sys.exec.env)
	}
}

func TestScriptFailureAndReasonHasNoScriptText(t *testing.T) {
	r := newRig(t)
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 30, Script: "echo TOPSECRET; exit 3"})
	if code := Run(r.config(true, "/bin/true"), r.sys); code != ExitFailed {
		t.Fatalf("code %d", code)
	}
	m := r.message()
	if m.Reason != "the script exited with code 3" || strings.Contains(m.Reason, "TOPSECRET") {
		t.Fatalf("message %+v", m)
	}
	raw, _ := os.ReadFile(r.log)
	if strings.Contains(string(raw), "TOPSECRET") {
		t.Fatal("script output leaked into the termination message")
	}
	if r.has("ok") || r.has("settled") || r.has("started") {
		t.Fatal("a failed attempt writes no success marker")
	}
}

func TestScriptTimeout(t *testing.T) {
	r := newRig(t)
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 1, Script: "sleep 30"})
	start := time.Now()
	if code := Run(r.config(true, "/bin/true"), r.sys); code != ExitFailed {
		t.Fatalf("code %d", code)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the time limit was not enforced")
	}
	if m := r.message(); m.Reason != "the script did not finish in 1 s" {
		t.Fatalf("message %+v", m)
	}
}

func TestMissingInterpreter(t *testing.T) {
	r := newRig(t)
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 5, Script: "true", Interpreter: "/no/such/interpreter"})
	if code := Run(r.config(true, "/bin/true"), r.sys); code != ExitFailed {
		t.Fatalf("code %d", code)
	}
	if m := r.message(); m.Reason != "the script interpreter cannot be started" {
		t.Fatalf("message %+v", m)
	}
}

// Three attempts with growing pauses, the third is final; after it the launcher waits instead of failing again.
func TestThreeAttemptsThenWait(t *testing.T) {
	r := newRig(t)
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 30, Script: "exit 1"})
	for i := 1; i <= 3; i++ {
		if code := Run(r.config(true, "/bin/true"), r.sys); code != ExitFailed {
			t.Fatalf("attempt %d: code %d", i, code)
		}
		m := r.message()
		if m.Attempt != i || m.Final != (i == 3) {
			t.Fatalf("attempt %d: message %+v", i, m)
		}
	}
	if !slices.Equal(r.sys.slept, []time.Duration{10 * time.Second, 30 * time.Second}) {
		t.Fatalf("pauses %v", r.sys.slept)
	}
	if code := Run(r.config(true, "/bin/true"), r.sys); code != 0 || !r.sys.paused {
		t.Fatalf("the device that gave up must wait, code %d paused %v", code, r.sys.paused)
	}
	if !r.has("settled") || r.has("ok") {
		t.Fatal("a failed device is settled (startup probe passes) but never ok (never ready)")
	}
	if r.sys.exec.path != "" {
		t.Fatal("the service must never start after a failed bootstrap")
	}
}

func TestExplicitAttemptNumberFromTheController(t *testing.T) {
	r := newRig(t)
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 30, Script: "exit 1"})
	cfg := r.config(true, "/bin/true")
	cfg.Attempt = 3
	if code := Run(cfg, r.sys); code != ExitFailed {
		t.Fatal(code)
	}
	if m := r.message(); m.Attempt != 3 || !m.Final {
		t.Fatalf("%+v", m)
	}
	if r.has("attempts") {
		t.Fatal("an explicit attempt does not use the counter")
	}
}

// A container restarted in place after the service ended has a fresh file system and no list: it asks for a new pod.
func TestRestartAfterServiceAsksForRecreation(t *testing.T) {
	r := newRig(t)
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 30})
	if code := Run(r.config(true, "/bin/true"), r.sys); code != 0 {
		t.Fatal(code)
	}
	r.sys.exec.path = ""
	if code := Run(r.config(true, "/bin/true"), r.sys); code != ExitRecycle {
		t.Fatalf("code %d", code)
	}
	if m := r.message(); m.Kind != KindRecycle {
		t.Fatalf("%+v", m)
	}
	if r.sys.exec.path != "" || r.has("ok") || r.has("settled") {
		t.Fatal("a recycled container must not run the service nor look ready")
	}
}

// A pod that starts from a saved state skips the bootstrap; it does not touch the directory.
func TestSkipOnSavedState(t *testing.T) {
	r := newRig(t)
	r.list(labchecks.Bootstrap{TimeLimitSeconds: 30, Files: []labchecks.File{{Path: "/etc/x", Content: []byte("v")}}})
	if code := Run(r.config(false, "/bin/true"), r.sys); code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(filepath.Join(r.root, "etc/x")); err == nil {
		t.Fatal("a skipped bootstrap placed a file")
	}
	if r.sys.exec.path == "" {
		t.Fatal("the service did not start")
	}
}

func TestServiceUserDrop(t *testing.T) {
	r := newRig(t)
	cfg := r.config(false, "/bin/true", "arg")
	cfg.User = "web"
	cfg.WorkDir = "/srv/app"
	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	if code := Run(cfg, r.sys); code != 0 {
		t.Fatal(code)
	}
	want := Identity{UID: 1000, GID: 1000, Home: "/home/web", Groups: []int{50, 60}}
	if r.sys.dropped == nil || r.sys.dropped.UID != want.UID || r.sys.dropped.GID != want.GID || !slices.Equal(r.sys.dropped.Groups, want.Groups) {
		t.Fatalf("dropped %+v", r.sys.dropped)
	}
	if !slices.Contains(r.sys.exec.env, "HOME=/home/web") || slices.Contains(r.sys.exec.env, "HOME=/root") || !slices.Contains(r.sys.exec.env, "FROM_IMAGE=1") {
		t.Fatalf("env %v", r.sys.exec.env)
	}
	if st, err := os.Stat(filepath.Join(r.root, "srv/app")); err != nil || !st.IsDir() {
		t.Fatalf("working directory not created: %v", err)
	}
	if got := r.sys.chowns["app"]; got != ([2]int{1000, 1000}) {
		t.Fatalf("working directory owner %v", got)
	}
}

func TestExplicitHomeIsKept(t *testing.T) {
	got := homeFor([]string{"HOME=/data"}, Identity{UID: 5, Home: "/home/x"})
	if !slices.Equal(got, []string{"HOME=/data"}) {
		t.Fatal(got)
	}
	got = homeFor([]string{"A=1"}, Identity{UID: 5})
	if !slices.Equal(got, []string{"A=1", "HOME=/"}) {
		t.Fatal(got)
	}
}

func TestRootServiceIsNotDropped(t *testing.T) {
	r := newRig(t)
	cfg := r.config(false, "/bin/true")
	cfg.User = "root"
	if Run(cfg, r.sys) != 0 || r.sys.dropped != nil {
		t.Fatalf("root stays root: %+v", r.sys.dropped)
	}
}

func TestUnknownServiceUserAndMissingCommand(t *testing.T) {
	r := newRig(t)
	cfg := r.config(false, "/bin/true")
	cfg.User = "ghost"
	if Run(cfg, r.sys) != ExitStart || r.message().Kind != KindStart || r.sys.exec.path != "" {
		t.Fatal("an unknown user must stop the start")
	}
	cfg = r.config(false, "definitely-not-a-program")
	if Run(cfg, r.sys) != ExitStart {
		t.Fatal("an unknown program must stop the start")
	}
	if Run(r.config(false), r.sys) != ExitStart {
		t.Fatal("no command must stop the start")
	}
	r.sys.execErr = errors.New("boom")
	if Run(r.config(false, "/bin/true"), r.sys) != ExitStart {
		t.Fatal("exec failure must be reported")
	}
}

func TestNumericUserNotInPasswd(t *testing.T) {
	r := newRig(t)
	id, err := Accounts{Root: r.root}.Resolve("4242:50")
	if err != nil || id.UID != 4242 || id.GID != 50 || len(id.Groups) != 0 {
		t.Fatalf("%+v %v", id, err)
	}
	id, err = Accounts{Root: r.root}.Resolve("web:staff")
	if err != nil || id.UID != 1000 || id.GID != 50 {
		t.Fatalf("%+v %v", id, err)
	}
	if _, err := (Accounts{Root: r.root}).Resolve("web:ghost"); err == nil {
		t.Fatal("unknown group")
	}
}

func TestStageCopiesTheListPrivately(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bootstrap.json")
	write(t, src, `{"script":"x"}`)
	dir := t.TempDir()
	if err := Stage(src, dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "bootstrap.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftovers %v", entries)
	}
	if err := Stage(filepath.Join(dir, "nope"), dir); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("error must not name paths: %v", err)
	}
}

func TestParseMessage(t *testing.T) {
	if _, ok := ParseMessage("OOMKilled"); ok {
		t.Fatal("foreign text")
	}
	if m, ok := ParseMessage(`{"kind":"failed","attempt":2,"final":true,"reason":"r"}`); !ok || m.Attempt != 2 || !m.Final {
		t.Fatal(m)
	}
}
