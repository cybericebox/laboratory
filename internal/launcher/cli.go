package launcher

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cybericebox/laboratory/internal/names"
)

// Main is the "launch" command of the in-device program:
//
//	probe launch [-bootstrap] [-attempt N] [-dir D] [-user U] [-workdir W] -- COMMAND [ARGS...]
//
// It returns an exit code only when the service did not start.
func Main(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := Config{}
	fs.BoolVar(&cfg.Bootstrap, "bootstrap", false, "run the bootstrap list first")
	fs.IntVar(&cfg.Attempt, "attempt", 0, "number of this attempt; 0 = count in the directory")
	fs.StringVar(&cfg.Dir, "dir", names.BootstrapDir, "directory with the bootstrap list")
	fs.StringVar(&cfg.User, "user", "", "service user (name, uid, name:group, uid:gid); empty is root")
	fs.StringVar(&cfg.WorkDir, "workdir", "", "working directory of the service")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	cfg.Command = fs.Args()
	if len(cfg.Command) == 0 {
		_, _ = fmt.Fprintln(stderr, "launch: no service command")
		return ExitUsage
	}
	return Run(cfg, OS{})
}

// Stage copies the list from the mounted Secret (src) into the memory volume dir, for the launcher. It is the command of
// the init container, the only container that sees the Secret. The copy is readable by root only.
func Stage(src, dir string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return errors.New("cannot read the bootstrap list")
	}
	defer clear(b)
	tmp, err := os.CreateTemp(dir, ".stage-*")
	if err != nil {
		return errors.New("cannot write the bootstrap list")
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return errors.New("cannot write the bootstrap list")
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return errors.New("cannot write the bootstrap list")
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return errors.New("cannot write the bootstrap list")
	}
	if err := os.Rename(name, filepath.Join(dir, listFile)); err != nil {
		_ = os.Remove(name)
		return errors.New("cannot write the bootstrap list")
	}
	return nil
}
