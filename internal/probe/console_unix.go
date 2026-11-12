//go:build unix

package probe

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// runResolveUser answers {"uid":..,"gid":..,"groups":[..],"home":".."} for "name", "uid", "name:group" or "uid:gid", looked up in the
// passwd and group files of the device the way a login would.
func runResolveUser(spec string) int {
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	passwd := readIDFile("/etc/passwd")
	var uid, gid uint32
	home := ""
	found := false
	if n, err := strconv.ParseUint(userPart, 10, 32); err == nil {
		uid = uint32(n)
		for _, f := range passwd {
			if len(f) >= 6 && f[2] == userPart {
				gid, home, found = parseID(f[3]), f[5], true
				break
			}
		}
		// A number without a passwd entry is still a user; it has no home and the group 0 unless one is named.
	} else {
		for _, f := range passwd {
			if len(f) >= 6 && f[0] == userPart {
				uid, gid, home, found = parseID(f[2]), parseID(f[3]), f[5], true
				break
			}
		}
		if !found {
			return ExitNoUser
		}
	}
	groups := readIDFile("/etc/group")
	if hasGroup {
		if n, err := strconv.ParseUint(groupPart, 10, 32); err == nil {
			gid = uint32(n)
		} else {
			ok := false
			for _, f := range groups {
				if len(f) >= 3 && f[0] == groupPart {
					gid, ok = parseID(f[2]), true
					break
				}
			}
			if !ok {
				return ExitNoUser
			}
		}
	}
	out := struct {
		UID    uint32   `json:"uid"`
		GID    uint32   `json:"gid"`
		Groups []uint32 `json:"groups,omitempty"`
		Home   string   `json:"home,omitempty"`
	}{UID: uid, GID: gid, Home: home}
	if found && !hasGroup {
		// Supplementary groups: the groups that list the user by name.
		for _, f := range groups {
			if len(f) >= 4 && f[0] != "" {
				for _, m := range strings.Split(f[3], ",") {
					if m == userPart && len(out.Groups) < 64 {
						out.Groups = append(out.Groups, parseID(f[2]))
					}
				}
			}
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return ExitIO
	}
	return 0
}

func parseID(s string) uint32 {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

// readIDFile reads a passwd- or group-style file (at most MaxIDFile bytes) into its colon-separated fields.
func readIDFile(path string) [][]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out [][]string
	sc := bufio.NewScanner(io.LimitReader(f, MaxIDFile))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		out = append(out, strings.Split(line, ":"))
	}
	return out
}

// exitFor maps a file-system error to the exit code the service understands.
func exitFor(err error) int {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ExitNotFound
	case errors.Is(err, fs.ErrPermission):
		return ExitDenied
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ENXIO):
		return ExitNotRegular
	}
	return ExitIO
}

// runPut writes standard input to the file. The file is opened without following a link in the last component and must be a regular
// file (or is created as one): a device node, a FIFO or a directory is refused. It runs with the rights of the console's user.
func runPut(path string) int {
	// O_NONBLOCK so that opening a FIFO that nobody reads cannot hang; the type is checked on the open descriptor.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return exitFor(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return ExitIO
	}
	if !st.Mode().IsRegular() {
		return ExitNotRegular
	}
	if err := f.Truncate(0); err != nil {
		return exitFor(err)
	}
	if _, err := io.Copy(f, os.Stdin); err != nil {
		return exitFor(err)
	}
	if err := f.Close(); err != nil {
		return ExitIO
	}
	return 0
}

// runGet writes the size and the content of a regular file. The size is read from the open descriptor, so the header and the
// content are of the same file; if the file grows meanwhile, only the announced length is sent.
func runGet(path string) int {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return exitFor(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return ExitIO
	}
	if !st.Mode().IsRegular() {
		return ExitNotRegular
	}
	var head [8]byte
	binary.BigEndian.PutUint64(head[:], uint64(st.Size()))
	if _, err := os.Stdout.Write(head[:]); err != nil {
		return ExitIO
	}
	n, err := io.Copy(os.Stdout, io.LimitReader(f, st.Size()))
	if err != nil || n != st.Size() {
		return ExitIO
	}
	return 0
}
