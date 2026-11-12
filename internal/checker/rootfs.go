//go:build unix

package checker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cybericebox/laboratory/internal/probe"
)

// RootFS reads files of a container strictly inside the container's root directory, as seen from the node, without running
// anything in the container.
//
// The participant owns that tree and is root in it: a symbolic link planted there must never make the checker read (or stat) a
// file of the node. Path resolution is therefore done here, one component at a time, and never by the kernel's path lookup:
//
//   - every step is an operation relative to a directory descriptor we hold (fstatat, openat, readlinkat) with no-follow flags, so
//     the kernel never follows a link by itself;
//   - a symbolic link is read and its target is resolved by this code, with an absolute target restarting at the root descriptor
//     (chroot semantics: an absolute link to /etc/shadow reaches the container's /etc/shadow);
//   - ".." is applied to our own stack of descriptors and never goes above the root, so it needs no kernel help and cannot be
//     raced by a participant that renames directories;
//   - the number of links followed is bounded;
//   - the last step opens the file with O_NOFOLLOW and O_NONBLOCK and looks at the opened descriptor (a regular file is required
//     for content), so a link or a FIFO swapped in at the last moment is refused, not followed or waited on.
//
// The tree the node sees is the container's root file system only (image layers and the writable layer). The mounts the
// container makes for itself (/proc, /sys, /dev, volumes) are not part of it; the top-level directories proc, sys and dev are
// refused so a check never reports on an empty mount point as if it were the real thing.
type RootFS struct {
	root int
	// MaxContent bounds the bytes read of a file.
	MaxContent int64
}

// Errors of path resolution. ErrEscape is never returned for a path that stays inside the root; a link that points outside is
// resolved against the root instead, so the only way to see it is a refusal of a pseudo file system path.
var (
	// ErrPseudoPath is returned for a path below /proc, /sys or /dev of the container.
	ErrPseudoPath = errors.New("the path is on a pseudo file system of the container, which the node cannot see; use a custom check")
	// ErrTooManyLinks is returned when resolution follows more symbolic links than maxLinks.
	ErrTooManyLinks = errors.New("too many symbolic links")
	// ErrChanged is returned when the tree changed under the reader again and again.
	ErrChanged = errors.New("the file kept changing while it was read")
)

const (
	maxLinks     = 40
	maxSteps     = 4096 // components looked at in one path, links included
	maxAttempts  = 4
	maxLinkBytes = 4096
)

// OpenRootFS opens the root directory of a container.
func OpenRootFS(dir string) (*RootFS, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open the container root: %w", err)
	}
	return &RootFS{root: fd, MaxContent: probe.MaxCheckFile}, nil
}

// Close releases the root descriptor.
func (r *RootFS) Close() { _ = unix.Close(r.root) }

// FileFacts is what a reader saw of one file. UID and GID are the numbers inside the container (host numbers are translated by
// the reader), User and Group their names when the device's /etc/passwd and /etc/group give one.
type FileFacts struct {
	Exists  bool
	Regular bool
	// Type is file, directory, symlink or other. The node-side reader follows links, so it never says symlink; the in-device tool does.
	Type string
	// Mode holds the permission bits including setuid, setgid and sticky.
	Mode      uint32
	UID, GID  uint32
	User      string
	Group     string
	Content   []byte
	Truncated bool
	// Unmapped is set when the owner is not in the id mapping of the container (the file belongs to nobody the container knows).
	Unmapped bool
}

// resolved is a directory descriptor and the name of an entry in it ("." for the directory itself).
type resolved struct {
	dir   int
	name  string
	close func()
}

// closeIntermediate closes the directories between the root and the last one of the stack.
func closeIntermediate(dirs []int) {
	for i := 1; i < len(dirs)-1; i++ {
		_ = unix.Close(dirs[i])
	}
}

func splitPath(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// resolve walks p inside the root and returns the directory holding the last component and its name. ok is false when the path
// does not exist (a missing component, or a component that is not a directory).
func (r *RootFS) resolve(p string) (res resolved, ok bool, err error) {
	queue := splitPath(p)
	dirs := []int{r.root} // dirs[0] is the root and is not ours to close
	closeAll := func() {
		for _, d := range dirs[1:] {
			_ = unix.Close(d)
		}
		dirs = dirs[:1]
	}
	fail := func(e error) (resolved, bool, error) { closeAll(); return resolved{}, false, e }
	links, steps := 0, 0
	for len(queue) > 0 {
		if steps++; steps > maxSteps {
			return fail(ErrTooManyLinks)
		}
		comp := queue[0]
		queue = queue[1:]
		cur := dirs[len(dirs)-1]
		switch comp {
		case ".":
			continue
		case "..":
			if len(dirs) > 1 {
				_ = unix.Close(dirs[len(dirs)-1])
				dirs = dirs[:len(dirs)-1]
			}
			continue
		}
		if len(dirs) == 1 && (comp == "proc" || comp == "sys" || comp == "dev") && len(queue) > 0 {
			return fail(ErrPseudoPath)
		}
		var st unix.Stat_t
		if err := unix.Fstatat(cur, comp, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
				closeAll()
				return resolved{}, false, nil
			}
			return fail(fmt.Errorf("look up a path component: %w", err))
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			if links++; links > maxLinks {
				return fail(ErrTooManyLinks)
			}
			buf := make([]byte, maxLinkBytes+1)
			n, err := unix.Readlinkat(cur, comp, buf)
			if err != nil {
				return fail(fmt.Errorf("read a symbolic link: %w", err))
			}
			if n > maxLinkBytes {
				return fail(ErrTooManyLinks)
			}
			target := string(buf[:n])
			if strings.HasPrefix(target, "/") {
				closeAll()
			}
			queue = append(splitPath(target), queue...)
			continue
		case unix.S_IFDIR:
			if len(queue) > 0 {
				fd, err := unix.Openat(cur, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if err != nil {
					if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
						closeAll()
						return resolved{}, false, nil
					}
					return fail(fmt.Errorf("enter a directory: %w", err))
				}
				dirs = append(dirs, fd)
				continue
			}
		default:
			if len(queue) > 0 {
				closeAll()
				return resolved{}, false, nil
			}
		}
		// The last component, not a link: report it relative to the directory we hold; the directories above it are not needed.
		closeIntermediate(dirs)
		return resolved{dir: cur, name: comp, close: func() {
			if cur != r.root {
				_ = unix.Close(cur)
			}
		}}, true, nil
	}
	// The path named a directory by itself (the root, or something ending in "." or ".."): that directory.
	keep := dirs[len(dirs)-1]
	closeIntermediate(dirs)
	return resolved{dir: keep, name: ".", close: func() {
		if keep != r.root {
			_ = unix.Close(keep)
		}
	}}, true, nil
}

// Observe looks at p and, when wantContent is set and p is a regular file, reads up to MaxContent bytes of it.
func (r *RootFS) Observe(p string, wantContent bool, ids IDMap) (FileFacts, error) {
	lastErr := ErrChanged
	for attempt := 0; attempt < maxAttempts; attempt++ {
		facts, retry, err := r.observeOnce(p, wantContent, ids)
		if !retry {
			return facts, err
		}
		lastErr = err
	}
	return FileFacts{}, lastErr
}

func (r *RootFS) observeOnce(p string, wantContent bool, ids IDMap) (facts FileFacts, retry bool, err error) {
	res, ok, err := r.resolve(p)
	if err != nil {
		return FileFacts{}, false, err
	}
	if !ok {
		return FileFacts{}, false, nil
	}
	defer res.close()
	var st unix.Stat_t
	if err := unix.Fstatat(res.dir, res.name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return FileFacts{}, false, nil
		}
		return FileFacts{}, false, fmt.Errorf("stat: %w", err)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		// Swapped for a link after the walk: walk again, so the link is resolved by us.
		return FileFacts{}, true, ErrChanged
	}
	facts = factsOf(&st, ids)
	if !wantContent || !facts.Regular {
		return facts, false, nil
	}
	fd, err := unix.Openat(res.dir, res.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return FileFacts{}, true, ErrChanged
		}
		return FileFacts{}, false, fmt.Errorf("open the file: %w", err)
	}
	f := os.NewFile(uintptr(fd), "checked file")
	defer func() { _ = f.Close() }()
	var ost unix.Stat_t
	if err := unix.Fstat(fd, &ost); err != nil {
		return FileFacts{}, false, fmt.Errorf("stat the open file: %w", err)
	}
	if ost.Mode&unix.S_IFMT != unix.S_IFREG || ost.Dev != st.Dev || ost.Ino != st.Ino {
		return FileFacts{}, true, ErrChanged
	}
	if pseudoFS(fd) {
		return FileFacts{}, false, ErrPseudoPath
	}
	buf, err := io.ReadAll(io.LimitReader(f, r.MaxContent+1))
	if err != nil {
		return FileFacts{}, false, fmt.Errorf("read the file: %w", err)
	}
	if int64(len(buf)) > r.MaxContent {
		buf, facts.Truncated = buf[:r.MaxContent], true
	}
	facts.Content = buf
	return facts, false, nil
}

// permBits are the permission, setuid, setgid and sticky bits of a mode (uint16 on some hosts, uint32 on Linux).
func permBits[T ~uint16 | ~uint32](m T) uint32 { return uint32(m) & 0o7777 }

func factsOf(st *unix.Stat_t, ids IDMap) FileFacts {
	f := FileFacts{Exists: true, Regular: st.Mode&unix.S_IFMT == unix.S_IFREG, Mode: permBits(st.Mode), Type: typeOfMode(st.Mode)}
	var uok, gok bool
	f.UID, uok = ids.UID(st.Uid)
	f.GID, gok = ids.GID(st.Gid)
	f.Unmapped = !uok || !gok
	return f
}

// ReadSmall reads a small file of the container (/etc/passwd) for name lookups; nil when it is missing or unreadable.
func (r *RootFS) ReadSmall(p string) []byte {
	small := *r
	small.MaxContent = probe.MaxIDFile
	f, err := small.Observe(p, true, IDMap{})
	if err != nil || !f.Regular {
		return nil
	}
	return f.Content
}

// Names fills in the user and group names of facts from the container's own /etc/passwd and /etc/group.
func (r *RootFS) Names(f *FileFacts) {
	names := probe.ParseIDNames(r.ReadSmall("/etc/passwd"), r.ReadSmall("/etc/group"))
	f.User, f.Group = names.Users[f.UID], names.Groups[f.GID]
}

func typeOfMode[T ~uint16 | ~uint32](mode T) string {
	switch uint32(mode) & uint32(unix.S_IFMT) {
	case unix.S_IFREG:
		return "file"
	case unix.S_IFDIR:
		return "directory"
	case unix.S_IFLNK:
		return "symlink"
	}
	return "other"
}
