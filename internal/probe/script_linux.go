//go:build linux

package probe

import (
	"os"

	"golang.org/x/sys/unix"
)

// heldScript is a script ready to be handed to an interpreter.
type heldScript struct {
	path    string
	extra   []*os.File
	release func()
}

// holdScript keeps the script in a memory file handed to the interpreter as /proc/self/fd/3: the root file system of the device
// may be read-only, and the script is never put where the participant could read it from or change it in.
func holdScript(script string) (heldScript, error) {
	fd, err := unix.MemfdCreate("check-script", 0)
	if err != nil {
		return heldScript{}, err
	}
	mf := os.NewFile(uintptr(fd), "check-script")
	if _, err := mf.WriteString(script); err != nil {
		_ = mf.Close()
		return heldScript{}, err
	}
	if err := mf.Chmod(0o444); err != nil {
		_ = mf.Close()
		return heldScript{}, err
	}
	return heldScript{path: "/proc/self/fd/3", extra: []*os.File{mf}, release: func() { _ = mf.Close() }}, nil
}
