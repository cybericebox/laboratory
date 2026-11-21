//go:build linux

package deviceaccess

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// ListenChecker makes the checker socket: a unix socket at path, mode 0660, group gid (the group the checker pod runs as; the service pod is a
// member of it, which is what lets a user without capabilities hand a file it owns to that group). A connection whose peer is not the
// checker's user (peerUID, read from SO_PEERCRED) is closed at once, whatever the file permissions say.
func ListenChecker(path string, gid, peerUID int) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// a socket left by a previous run
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	// The mode is set before the socket exists for anyone: a process-wide umask keeps it from being group or world writable for a moment.
	old := syscall.Umask(0o117)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if err := os.Chown(path, -1, gid); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("give the socket to group %d: %w", gid, err)
	}
	return checkedListener{Listener: ln, check: peerIs(peerUID, gid)}, nil
}

// peerIs accepts a peer with the given uid and primary group.
func peerIs(uid, gid int) PeerCheck {
	return func(c net.Conn) error {
		uc, ok := c.(*net.UnixConn)
		if !ok {
			return errors.New("not a unix connection")
		}
		raw, err := uc.SyscallConn()
		if err != nil {
			return err
		}
		var cred *syscall.Ucred
		var cerr error
		if err := raw.Control(func(fd uintptr) {
			cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		}); err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
		if int(cred.Uid) != uid || int(cred.Gid) != gid {
			return errors.New("the peer is not the checker")
		}
		return nil
	}
}
