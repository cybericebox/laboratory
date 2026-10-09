package devicestate

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"strings"

	"github.com/containerd/containerd/v2/pkg/archive/tarheader"
	"github.com/cybericebox/laboratory/internal/snapshot"
	"golang.org/x/sys/unix"
)

const (
	xattrPAXPrefix        = "SCHILY.xattr."
	xattrBookkeepingMax   = 32 << 20
	xattrLayerMetadataMax = 64 << 20
)

type xattrInode struct{ dev, ino uint64 }

type userXattrLayer struct {
	ctx                context.Context
	pol                snapshot.Policy
	upper, live, lower *os.Root
	tw                 *tar.Writer
	seen               map[[32]byte]bool
	skipped            map[[32]byte]bool
	links              map[xattrInode]string
	entries, visited   int
	bytes, metadata    int64
	metadataLimit      int64
	bookkeeping        int
}

// writeUserXattrLayer retains containerd's changeset and supplements user.*
// metadata that its comparer/writer omit. Roots are pinned read-only snapshots;
// the caller holds quiescence until this stream finishes. Only one entry's
// bounded attributes are in memory. Path/inode bookkeeping has a separate 32 MiB
// admission cap; exported user metadata is bounded by WriteQuota and 64 MiB.
// These guards do not change snapshot.Stats.Bytes (regular payload accounting).
func writeUserXattrLayer(ctx context.Context, raw io.Reader, out io.Writer, upper, live, lower *os.Root, pol snapshot.Policy) error {
	l := &userXattrLayer{ctx: ctx, pol: pol, upper: upper, live: live, lower: lower, tw: tar.NewWriter(out), seen: map[[32]byte]bool{}, skipped: map[[32]byte]bool{}, links: map[xattrInode]string{}, metadataLimit: xattrLayerMetadataMax}
	if pol.WriteQuota > 0 && pol.WriteQuota < l.metadataLimit {
		l.metadataLimit = pol.WriteQuota
	}
	tr := tar.NewReader(contextReader{ctx, raw})
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := xattrRootName(h.Name)
		if err != nil {
			return err
		}
		if pol.Excluded(xattrLogicalPath(name)) {
			continue
		}
		if err := l.remember(name); err != nil {
			return err
		}
		// Entries the filter already refuses/skips reach it unchanged, without
		// live metadata reads or new quota charges. Keep its original order.
		if h.Typeflag == tar.TypeChar || h.Typeflag == tar.TypeBlock || h.Typeflag == tar.TypeFifo || layerHeaderBytes(h) > snapshot.MaxEntryHeaderBytes || l.policySkipped(h) {
			if err := l.writeSkipped(h); err != nil {
				return err
			}
			if h.Typeflag == tar.TypeReg {
				if _, err := io.Copy(l.tw, contextReader{ctx, tr}); err != nil {
					return err
				}
			}
			continue
		}
		var attrs map[string]string
		if !strings.HasPrefix(path.Base(name), ".wh.") && (h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeDir || h.Typeflag == tar.TypeLink) {
			f, info, stat, err := openXattrFile(live, name)
			if err != nil {
				return err
			}
			if int(stat.Uid) != h.Uid || int(stat.Gid) != h.Gid || h.Typeflag == tar.TypeReg && info.Size() != h.Size || h.Typeflag == tar.TypeDir && !info.IsDir() {
				_ = f.Close()
				return fmt.Errorf("snapshot inode metadata changed for %q", name)
			}
			attrs, err = readUserXattrs(ctx, f, snapshot.MaxEntryHeaderBytes-layerHeaderBytes(h))
			if err == nil && info.IsDir() {
				err = l.checkDirectoryRemoval(name, attrs)
			}
			if err == nil && !stableXattrFile(f, stat) {
				err = fmt.Errorf("snapshot inode changed while reading attributes for %q", name)
			}
			if err == nil && h.Typeflag == tar.TypeReg && stat.Nlink > 1 {
				err = l.rememberLink(stat, name)
			}
			_ = f.Close()
			if err != nil {
				return err
			}
			replaceUserXattrs(h, attrs)
		}
		if err := l.writeHeader(h, attrs); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.Copy(l.tw, contextReader{ctx, tr}); err != nil {
				return err
			}
		}
	}
	if err := l.walkUpper(".", 0); err != nil {
		return err
	}
	return l.tw.Close()
}

func (l *userXattrLayer) remember(name string) error {
	key := sha256.Sum256([]byte(name))
	if !l.seen[key] {
		if err := l.reserveBookkeeping(128); err != nil {
			return err
		}
		l.seen[key] = true
	}
	return nil
}

func (l *userXattrLayer) reserveBookkeeping(n int) error {
	if n > xattrBookkeepingMax-l.bookkeeping {
		return fmt.Errorf("snapshot user xattr bookkeeping exceeds %d bytes", xattrBookkeepingMax)
	}
	l.bookkeeping += n
	return nil
}

func (l *userXattrLayer) rememberLink(st unix.Stat_t, name string) error {
	key := xattrInode{st.Dev, st.Ino}
	if _, ok := l.links[key]; !ok {
		if err := l.reserveBookkeeping(128 + len(name)); err != nil {
			return err
		}
		l.links[key] = name
	}
	return nil
}

func (l *userXattrLayer) writeHeader(h *tar.Header, attrs map[string]string) error {
	if err := l.ctx.Err(); err != nil {
		return err
	}
	if layerHeaderBytes(h) > snapshot.MaxEntryHeaderBytes {
		return fmt.Errorf("snapshot entry header exceeds %d bytes", snapshot.MaxEntryHeaderBytes)
	}
	if l.policySkipped(h) {
		return l.writeSkipped(h)
	}
	if l.pol.MaxEntries > 0 && l.entries >= l.pol.MaxEntries {
		return fmt.Errorf("%w: user xattr supplements exceed %d entries", snapshot.ErrEntries, l.pol.MaxEntries)
	}
	if h.Typeflag == tar.TypeReg && l.pol.WriteQuota > 0 && h.Size > l.pol.WriteQuota-l.bytes {
		return fmt.Errorf("%w: user xattr supplements exceed the payload quota", snapshot.ErrQuota)
	}
	var metadata int64
	for k, v := range attrs {
		metadata += int64(32 + len(k) + len(v)) // includes bounded PAX record overhead
	}
	if metadata > l.metadataLimit-l.metadata {
		return fmt.Errorf("%w: user xattr metadata exceeds %d bytes", snapshot.ErrQuota, l.metadataLimit)
	}
	if err := l.tw.WriteHeader(h); err != nil {
		return err
	}
	l.metadata += metadata
	l.entries++
	if h.Typeflag == tar.TypeReg {
		l.bytes += h.Size
	}
	return nil
}

func (l *userXattrLayer) policySkipped(h *tar.Header) bool {
	return h.Typeflag == tar.TypeReg && l.pol.MaxFileSize > 0 && h.Size > l.pol.MaxFileSize || h.Typeflag == tar.TypeLink && l.skipped[sha256.Sum256([]byte(snapshot.CleanPath(h.Linkname)))]
}

func (l *userXattrLayer) writeSkipped(h *tar.Header) error {
	if h.Typeflag == tar.TypeReg && l.pol.MaxFileSize > 0 && h.Size > l.pol.MaxFileSize && layerHeaderBytes(h) <= snapshot.MaxEntryHeaderBytes {
		key := sha256.Sum256([]byte(xattrLogicalPath(h.Name)))
		if !l.skipped[key] {
			if err := l.reserveBookkeeping(128); err != nil {
				return err
			}
			l.skipped[key] = true
		}
	}
	return l.tw.WriteHeader(h)
}

func (l *userXattrLayer) checkDirectoryRemoval(name string, attrs map[string]string) error {
	old, directory, err := l.parentAttrs(name)
	if err != nil {
		return err
	}
	if !directory {
		return nil
	}
	for k := range old {
		if _, ok := attrs[k]; !ok {
			// containerd merges directory entries and only sets archived xattrs.
			// Omitting an old key would leave it behind after restore.
			return fmt.Errorf("snapshot cannot preserve directory user xattr removal at %q", name)
		}
	}
	return nil
}

func (l *userXattrLayer) parentAttrs(name string) (map[string]string, bool, error) {
	pin, err := l.lower.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	info, err := pin.Stat()
	_ = pin.Close()
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, false, nil // Linux user.* attrs cannot exist on these inodes
	}
	f, _, _, err := openXattrFile(l.lower, name)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	attrs, err := readUserXattrs(l.ctx, f, snapshot.MaxEntryHeaderBytes-len(name))
	return attrs, info.IsDir(), err
}

func (l *userXattrLayer) walkUpper(name string, depth int) error {
	if err := l.ctx.Err(); err != nil {
		return err
	}
	if l.pol.Excluded(snapshot.CleanPath(name)) {
		return nil
	}
	if len(name) > snapshot.MaxEntryHeaderBytes || depth > 256 {
		return fmt.Errorf("snapshot upper path exceeds metadata traversal bounds")
	}
	l.visited++
	if l.visited > xattrBookkeepingMax/128 {
		return fmt.Errorf("snapshot upper metadata traversal exceeds its bounded entry budget")
	}
	pin, err := l.upper.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	info, err := pin.Stat()
	_ = pin.Close()
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil // overlay whiteout devices, symlinks and other special inodes
	}
	if !l.seen[sha256.Sum256([]byte(name))] {
		if err := l.supplement(name); err != nil {
			return err
		}
	}
	if !info.IsDir() {
		return nil
	}
	f, _, _, err := openXattrFile(l.upper, name)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(128) // never allocate an entire large directory
		if err != nil && err != io.EOF {
			return err
		}
		for _, entry := range entries {
			if err := l.walkUpper(path.Join(name, entry.Name()), depth+1); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func (l *userXattrLayer) supplement(name string) error {
	f, info, stat, err := openXattrFile(l.live, name)
	if err != nil {
		return err
	}
	defer f.Close()
	attrs, err := readUserXattrs(l.ctx, f, snapshot.MaxEntryHeaderBytes-len(name))
	if err != nil {
		return err
	}
	old, _, err := l.parentAttrs(name)
	if err != nil {
		return err
	}
	if maps.Equal(attrs, old) {
		return nil
	}
	if info.IsDir() {
		if err := l.checkDirectoryRemoval(name, attrs); err != nil {
			return err
		}
	}
	h, err := tarheader.FileInfoHeaderNoLookups(info, "")
	if err != nil {
		return err
	}
	h.Name, h.Format = name, tar.FormatPAX
	if info.IsDir() {
		h.Name += "/"
	} else if stat.Nlink > 1 {
		if target, ok := l.links[xattrInode{stat.Dev, stat.Ino}]; ok {
			h.Typeflag, h.Size, h.Linkname = tar.TypeLink, 0, target
		} else if err := l.rememberLink(stat, name); err != nil {
			return err
		}
	}
	replaceUserXattrs(h, attrs)
	capability, err := readFileXattr(l.ctx, f, "security.capability", snapshot.MaxEntryHeaderBytes-layerHeaderBytes(h))
	if err != nil && !errors.Is(err, unix.ENODATA) {
		return err
	}
	if len(capability) > 0 {
		h.PAXRecords[xattrPAXPrefix+"security.capability"] = capability
	}
	if !stableXattrFile(f, stat) {
		return fmt.Errorf("snapshot inode changed while reading attributes for %q", name)
	}
	if err := l.writeHeader(h, attrs); err != nil {
		return err
	}
	if h.Typeflag == tar.TypeReg {
		if _, err := io.CopyN(l.tw, contextReader{l.ctx, f}, h.Size); err != nil {
			return err
		}
		if !stableXattrFile(f, stat) {
			return fmt.Errorf("snapshot inode changed while supplementing %q", name)
		}
	}
	return l.remember(name)
}

func xattrRootName(name string) (string, error) {
	clean := path.Clean(name)
	if path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("snapshot path is outside its root")
	}
	return clean, nil
}

// First pin the inode without opening devices/FIFOs, then reopen that exact
// descriptor for xattrs/payload. Root resolves intermediate symlinks safely;
// O_NOFOLLOW refuses final symlinks. No untrusted pathname is used for reopening.
func openXattrFile(root *os.Root, name string) (*os.File, os.FileInfo, unix.Stat_t, error) {
	var zero unix.Stat_t
	pin, err := root.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, zero, err
	}
	defer pin.Close()
	info, err := pin.Stat()
	if err != nil {
		return nil, nil, zero, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, nil, zero, fmt.Errorf("snapshot metadata path is not a regular file or directory")
	}
	f, err := os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", pin.Fd()), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, zero, err
	}
	opened, err := f.Stat()
	var stat unix.Stat_t
	if err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, nil, zero, fmt.Errorf("snapshot pinned inode identity changed")
	}
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		_ = f.Close()
		return nil, nil, zero, err
	}
	return f, opened, stat, nil
}

func stableXattrFile(f *os.File, before unix.Stat_t) bool {
	var after unix.Stat_t
	return unix.Fstat(int(f.Fd()), &after) == nil && before.Dev == after.Dev && before.Ino == after.Ino && before.Mode == after.Mode && before.Uid == after.Uid && before.Gid == after.Gid && before.Size == after.Size && before.Nlink == after.Nlink && before.Mtim == after.Mtim && before.Ctim == after.Ctim
}

func readUserXattrs(ctx context.Context, f *os.File, remaining int) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, err := unix.Flistxattr(int(f.Fd()), nil)
	if err != nil {
		return nil, err
	}
	if n > snapshot.MaxEntryHeaderBytes || remaining < 0 {
		return nil, fmt.Errorf("snapshot xattr names exceed the header budget")
	}
	names := make([]byte, n)
	n, err = unix.Flistxattr(int(f.Fd()), names)
	if err != nil {
		return nil, err
	}
	if n < 0 || n > len(names) {
		return nil, fmt.Errorf("snapshot xattr names changed while reading")
	}
	attrs := map[string]string{}
	for _, b := range bytes.Split(names[:n], []byte{0}) {
		name := string(b)
		if !strings.HasPrefix(name, "user.") {
			continue
		}
		remaining -= len(xattrPAXPrefix) + len(name)
		value, err := readFileXattr(ctx, f, name, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= len(value)
		attrs[name] = value
	}
	return attrs, nil
}

func readFileXattr(ctx context.Context, f *os.File, name string, remaining int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	n, err := unix.Fgetxattr(int(f.Fd()), name, nil)
	if err != nil {
		return "", err
	}
	if n > remaining || n > snapshot.MaxEntryHeaderBytes {
		return "", fmt.Errorf("snapshot xattr value exceeds the header budget")
	}
	value := make([]byte, n)
	n, err = unix.Fgetxattr(int(f.Fd()), name, value)
	if err != nil {
		return "", err
	}
	if n < 0 || n > len(value) {
		return "", fmt.Errorf("snapshot xattr value changed while reading")
	}
	return string(value[:n]), nil
}

func replaceUserXattrs(h *tar.Header, attrs map[string]string) {
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, xattrPAXPrefix+"user.") {
			delete(h.PAXRecords, k)
		}
	}
	for k := range h.Xattrs {
		if strings.HasPrefix(k, "user.") {
			delete(h.Xattrs, k)
		}
	}
	if h.PAXRecords == nil {
		h.PAXRecords = map[string]string{}
	}
	for k, v := range attrs {
		h.PAXRecords[xattrPAXPrefix+k] = v
	}
	if len(attrs) > 0 {
		h.Format = tar.FormatPAX
	}
}

func xattrLogicalPath(name string) string {
	dir, base := path.Split(name)
	if base == ".wh..wh..opq" {
		return snapshot.CleanPath(dir)
	}
	if deleted, ok := strings.CutPrefix(base, ".wh."); ok {
		return snapshot.CleanPath(dir + deleted)
	}
	return snapshot.CleanPath(name)
}

func layerHeaderBytes(h *tar.Header) int {
	n := len(h.Name) + len(h.Linkname) + len(h.Uname) + len(h.Gname)
	for k, v := range h.PAXRecords {
		n += len(k) + len(v)
	}
	for k, v := range h.Xattrs {
		n += len(k) + len(v)
	}
	return n
}

type contextReader struct {
	ctx context.Context
	in  io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(p)
}
