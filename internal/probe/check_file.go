//go:build unix

package probe

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// runCheckFile observes one file of the device the tool runs in. It follows symbolic links: the device is the participant's own
// world, there is nothing to protect inside it (the node-side reader of a container device is the one that must not be fooled).
func runCheckFile(req FileRequest) FileResponse {
	var resp FileResponse
	if req.Path == "" || req.Path[0] != '/' {
		resp.Error = "the path is not absolute"
		return resp
	}
	f, err := os.Open(req.Path)
	var st os.FileInfo
	if err == nil {
		defer func() { _ = f.Close() }()
		st, err = f.Stat()
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		// Not readable (a socket, a device node, no permission): fall back to what stat can tell.
		var serr error
		if st, serr = os.Stat(req.Path); serr == nil {
			err, f = nil, nil
		}
	}
	switch {
	case err != nil && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)):
		return resp
	case err != nil:
		resp.Error = "cannot look at the file: " + errnoName(err)
		return resp
	}
	resp.Exists = true
	resp.Regular = st.Mode().IsRegular()
	resp.Mode = uint32(st.Mode().Perm())
	if st.Mode()&fs.ModeSetuid != 0 {
		resp.Mode |= 0o4000
	}
	if st.Mode()&fs.ModeSetgid != 0 {
		resp.Mode |= 0o2000
	}
	if st.Mode()&fs.ModeSticky != 0 {
		resp.Mode |= 0o1000
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		resp.UID, resp.GID = sys.Uid, sys.Gid
	}
	names := ParseIDNames(readBounded("/etc/passwd", MaxIDFile), readBounded("/etc/group", MaxIDFile))
	resp.User, resp.Group = names.Users[resp.UID], names.Groups[resp.GID]
	if req.WantContent && resp.Regular && f != nil {
		buf, err := io.ReadAll(io.LimitReader(f, MaxCheckFile+1))
		if err != nil {
			resp.Error = "cannot read the file: " + errnoName(err)
			return resp
		}
		if len(buf) > MaxCheckFile {
			buf, resp.Truncated = buf[:MaxCheckFile], true
		}
		resp.Content = buf
	}
	return resp
}

// errnoName is the short name of an error for a result: the errno text, never a path.
func errnoName(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return "I/O error"
}
