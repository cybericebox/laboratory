package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/cybericebox/laboratory/internal/labchecks"
)

// placeFile writes one file of the list: the parents are created (root, 0755), the content goes to a temporary file in the
// same directory with the final owner and mode, and replaces the target by a rename, so a reader never sees half a file and
// a symbolic link at the target is replaced, not followed.
func placeFile(root string, acc Accounts, sys System, f labchecks.File) error {
	uid, err := acc.UID(f.Owner)
	if err != nil {
		return errors.New("unknown owner")
	}
	gid, err := acc.GID(f.Group)
	if err != nil {
		return errors.New("unknown group")
	}
	target := filepath.Join(root, filepath.Clean(f.Path))
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errors.New(errText(err))
	}
	mode := fileMode(f.Mode)
	tmp, err := os.CreateTemp(dir, ".cib-*")
	if err != nil {
		return errors.New(errText(err))
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(f.Content); err != nil {
		_ = tmp.Close()
		cleanup()
		return errors.New(errText(err))
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return errors.New(errText(err))
	}
	if err := sys.Chown(name, uid, gid); err != nil {
		cleanup()
		return errors.New(errText(err))
	}
	// chmod after chown: a chown clears setuid and setgid bits.
	if err := os.Chmod(name, mode); err != nil {
		cleanup()
		return errors.New(errText(err))
	}
	if err := os.Rename(name, target); err != nil {
		cleanup()
		return errors.New(errText(err))
	}
	return nil
}

// errText is the system error without the file name: a path is not part of a reason.
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		err = le.Err
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return "write failed"
}

func placeAll(root string, acc Accounts, sys System, files []labchecks.File) error {
	for i, f := range files {
		if err := placeFile(root, acc, sys, f); err != nil {
			return fmt.Errorf("file %d of %d: %w", i+1, len(files), err)
		}
	}
	return nil
}

// fileMode turns permission bits with setuid, setgid and sticky (as in chmod) into a FileMode; 0 is the default 0644.
func fileMode(bits uint32) fs.FileMode {
	if bits == 0 {
		return 0o644
	}
	m := fs.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if bits&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if bits&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}
