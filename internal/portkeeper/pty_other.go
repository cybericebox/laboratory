//go:build !linux

package portkeeper

import (
	"errors"
	"os"
)

// OS is the real System; the keeper runs in Linux devices only.
type OS struct{}

var errNotLinux = errors.New("the port keeper runs on Linux only")

func (OS) OpenPTY() (master, slave *os.File, name string, err error) {
	return nil, nil, "", errNotLinux
}
func (OS) MakeRaw(*os.File) error { return errNotLinux }
func (OS) StartConsole(string, []string, Identity, []string) (*Process, error) {
	return nil, errNotLinux
}

// BecomeSubreaper is Linux only.
func BecomeSubreaper() error { return errNotLinux }
