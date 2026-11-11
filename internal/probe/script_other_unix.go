//go:build unix && !linux

package probe

import "os"

type heldScript struct {
	path    string
	extra   []*os.File
	release func()
}

// holdScript is for development hosts that have no memory files: the script goes to a temporary file. Devices run Linux.
func holdScript(script string) (heldScript, error) {
	f, err := os.CreateTemp("", "check-script-*")
	if err != nil {
		return heldScript{}, err
	}
	name := f.Name()
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return heldScript{}, err
	}
	_ = f.Close()
	_ = os.Chmod(name, 0o644)
	return heldScript{path: name, release: func() { _ = os.Remove(name) }}, nil
}
