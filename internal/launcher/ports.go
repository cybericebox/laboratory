package launcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cybericebox/laboratory/internal/portkeeper"
)

// startPorts starts the port keeper of a device with console or serial ports (docs/internal-docs/laboratory/specs/
// console-ports-design.md, 6.2), right before the service: still as root, after the bootstrap. The serial devices are owned by the
// service user. With a stdio console the launcher opens its PTY: the keeper gets the master, and the slave is returned to become the
// service's stdin, stdout and stderr. The ports never keep the service from starting: what goes wrong is reported on stderr (the
// container log) and the device runs without its lines.
func (l *run) startPorts(service Identity) (stdio *os.File) {
	if l.cfg.Ports == "" {
		return nil
	}
	warn := func(format string, a ...any) {
		_, _ = fmt.Fprintf(os.Stderr, "launcher: console ports: "+format+"\n", a...)
	}
	path := filepath.Join(l.cfg.Root, l.cfg.Ports)
	raw, err := os.ReadFile(path)
	if err != nil {
		warn("the port list cannot be read")
		return nil
	}
	spec, err := portkeeper.Parse(raw)
	if err != nil {
		warn("%v", err)
		return nil
	}
	var extra []*os.File
	var master, slave *os.File
	if spec.Stdio() != nil {
		if master, slave, err = l.sys.OpenPTY(); err != nil {
			warn("the stdio console has no PTY: %v", err)
		} else {
			extra = append(extra, master)
		}
	}
	owner := &portkeeper.Identity{UID: service.UID, GID: service.GID}
	if err := l.sys.StartKeeper(portkeeper.KeeperArgs(path, owner, master != nil), extra); err != nil {
		warn("the port keeper does not start: %v", err)
		if master != nil {
			_ = master.Close()
			_ = slave.Close()
		}
		return nil
	}
	if master != nil {
		_ = master.Close() // the keeper holds it now
	}
	return slave
}
