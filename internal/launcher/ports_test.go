package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cybericebox/laboratory/internal/portkeeper"
)

var errTest = errors.New("test")

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func (r *rig) ports(spec string) string {
	r.t.Helper()
	path := "/.cib/portspec/ports.json"
	full := filepath.Join(r.root, path)
	if err := mkdirAll(filepath.Dir(full)); err != nil {
		r.t.Fatal(err)
	}
	write(r.t, full, spec)
	return path
}

// The launcher starts the port keeper right before it becomes the service, still as root: after the bootstrap, before the switch to the
// service user and the exec. The keeper gets the port list and the service user as the owner of the serial devices.
func TestPortKeeperStartsBeforeTheService(t *testing.T) {
	r := newRig(t)
	cfg := r.config(false, "/bin/true")
	cfg.User = "web"
	cfg.Ports = r.ports(`{"console":[{"name":"console0"}],"serial":[{"name":"serial0","path":"/dev/ttyCON0"}]}`)
	if code := Run(cfg, r.sys); code != 0 {
		t.Fatalf("code %d", code)
	}
	if !slices.Equal(r.sys.calls, []string{"keeper", "drop", "exec"}) {
		t.Fatalf("order %v", r.sys.calls)
	}
	want := portkeeper.KeeperArgs(filepath.Join(r.root, cfg.Ports), &portkeeper.Identity{UID: 1000, GID: 1000}, false)
	if !slices.Equal(r.sys.keeperArgs, want) || r.sys.keeperFDs != 0 || r.sys.ptyOpened != 0 {
		t.Fatalf("keeper args %v (want %v), fds %d, ptys %d", r.sys.keeperArgs, want, r.sys.keeperFDs, r.sys.ptyOpened)
	}
}

// A stdio console makes the line the service's own terminal: the launcher opens the PTY, gives the keeper the master and the service the
// slave as stdin, stdout and stderr.
func TestStdioConsoleIsTheServiceTerminal(t *testing.T) {
	r := newRig(t)
	cfg := r.config(false, "/bin/true")
	cfg.Ports = r.ports(`{"console":[{"name":"console0","native":"stdio"}]}`)
	if code := Run(cfg, r.sys); code != 0 {
		t.Fatalf("code %d", code)
	}
	if r.sys.ptyOpened != 1 || r.sys.keeperFDs != 1 || r.sys.stdio == nil || !slices.Contains(r.sys.keeperArgs, "-stdio-fd") {
		t.Fatalf("ptys %d fds %d stdio %v args %v", r.sys.ptyOpened, r.sys.keeperFDs, r.sys.stdio, r.sys.keeperArgs)
	}
	if !slices.Equal(r.sys.calls, []string{"keeper", "stdio", "exec"}) {
		t.Fatalf("order %v", r.sys.calls)
	}
}

// The ports never keep the service from starting: a missing or invalid list, or a keeper that does not start, leaves the device without
// its lines but running.
func TestServiceStartsWithoutItsPorts(t *testing.T) {
	for name, spec := range map[string]string{"invalid": `{"console":[{"name":"tty0"}]}`, "unknown key": `{"shell":true}`} {
		r := newRig(t)
		cfg := r.config(false, "/bin/true")
		cfg.Ports = r.ports(spec)
		if code := Run(cfg, r.sys); code != 0 || r.sys.exec.path == "" || slices.Contains(r.sys.calls, "keeper") {
			t.Fatalf("%s: code %d calls %v", name, code, r.sys.calls)
		}
	}
	r := newRig(t)
	cfg := r.config(false, "/bin/true")
	cfg.Ports = "/.cib/portspec/missing.json"
	if code := Run(cfg, r.sys); code != 0 || r.sys.exec.path == "" {
		t.Fatalf("missing list: code %d", code)
	}
	r = newRig(t)
	r.sys.keeperErr = errTest
	cfg = r.config(false, "/bin/true")
	cfg.Ports = r.ports(`{"console":[{"name":"console0","native":"stdio"}]}`)
	if code := Run(cfg, r.sys); code != 0 || r.sys.exec.path == "" || r.sys.stdio != nil {
		t.Fatalf("keeper failure: code %d stdio %v", code, r.sys.stdio)
	}
}
