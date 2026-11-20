package snapshot

import (
	"archive/tar"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// entryPath is the logical path an entry refers to: a whiteout ".wh.x" in
// directory d refers to d/x, an opaque marker in d refers to d.
func entryPath(name string) (logical string, whiteout, opaque bool) {
	p := CleanPath(name)
	dir, base := path.Split(p)
	switch {
	case base == whiteoutOpaque:
		return CleanPath(dir), false, true
	case strings.HasPrefix(base, whiteoutPrefix):
		return CleanPath(dir + strings.TrimPrefix(base, whiteoutPrefix)), true, false
	}
	return p, false, false
}

// Stats describe a filtered or merged layer.
type Stats struct {
	// Entries is the number of tar entries written.
	Entries int
	// Bytes is the total size of the regular files written (uncompressed).
	Bytes int64
	// Dropped is the number of entries left out.
	Dropped int
	// RefusedEntries counts omitted data outside explicitly excluded paths.
	RefusedEntries int
	// Skipped are the first MaxSkippedListed regular files left out for being larger than
	// the policy's MaxFileSize; SkippedTotal counts all of them.
	Skipped      []SkippedFile
	SkippedTotal int
	// Unmapped counts the owner ids that were outside the user namespace map (written as 0).
	Unmapped int
}

// SkippedFile is a regular file left out of a snapshot for its size.
type SkippedFile struct {
	Path string
	Size int64
}

// MaxSkippedListed caps the skipped files named in a warning.
const MaxSkippedListed = 10

// Bounds of what a participant's file names may put into a status warning: one path is quoted and cut at MaxWarningPath bytes, a whole
// warning at MaxWarningBytes (see TruncateWarning).
const (
	MaxWarningPath  = 128
	MaxWarningBytes = 1024
)

// QuotePath is a path as a participant's file names may appear in a message: quoted (a newline or a quote in the name cannot forge a
// second line or end the quotation) and cut at MaxWarningPath bytes on a character boundary, with an ellipsis after the quote when cut.
func QuotePath(p string) string {
	cut := false
	if len(p) > MaxWarningPath {
		p, cut = p[:MaxWarningPath], true
		for len(p) > 0 && !utf8.ValidString(p) {
			p = p[:len(p)-1]
		}
	}
	q := strconv.Quote(p)
	if cut {
		q += "…"
	}
	return q
}

// TruncateWarning cuts a warning at MaxWarningBytes on a character boundary, with an ellipsis when cut.
func TruncateWarning(msg string) string {
	if len(msg) <= MaxWarningBytes {
		return msg
	}
	msg = msg[:MaxWarningBytes-len("…")]
	for len(msg) > 0 && !utf8.ValidString(msg) {
		msg = msg[:len(msg)-1]
	}
	return msg + "…"
}

// SkippedWarning is the status warning for the files skipped for size; empty when none. Paths are the participant's, so they are quoted and
// cut, and the whole message is bounded.
func (s Stats) SkippedWarning(maxFileSize int64) string {
	if s.SkippedTotal == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.Skipped))
	for _, f := range s.Skipped {
		parts = append(parts, fmt.Sprintf("%s (%d bytes)", QuotePath(f.Path), f.Size))
	}
	msg := fmt.Sprintf("files over %d bytes are not kept: %s", maxFileSize, strings.Join(parts, ", "))
	if more := s.SkippedTotal - len(s.Skipped); more > 0 {
		msg += fmt.Sprintf(" and %d more", more)
	}
	return TruncateWarning(msg)
}

// FilterLayer copies an uncompressed layer tar from in to out, leaving out the
// entries (and whiteouts) of excluded paths and the regular files larger than the
// policy's MaxFileSize (with the hard links to them). Whiteouts are empty files and
// never skipped for size. The result is a complete tar.
func FilterLayer(in io.Reader, out io.Writer, pol Policy) (Stats, error) {
	return FilterLayerMapped(in, out, pol, IDMaps{})
}

// FilterLayerMapped is FilterLayer for a container in a user namespace: the diff holds the host ids of the files (the shifted ids of that
// pod's namespace), and the snapshot must hold the ids inside the container, so every owner is translated through ids. An id outside
// the map is written as 0 and counted in Stats.Unmapped. Containerd's synthetic root-owned whiteout markers already carry
// container-relative zero ids and are not translated. Empty maps change nothing.
func FilterLayerMapped(in io.Reader, out io.Writer, pol Policy, ids IDMaps) (Stats, error) {
	var st Stats
	// emitted are the paths written to the output (at most MaxEntries of them). A hard link is kept only when its target is among them: a
	// target dropped for any reason (an excluded path, a refused type or header, a file over the size limit, a name that is not a layer
	// entry) would leave the link dangling, and a layer with a dangling link fails to unpack on restore.
	emitted := map[string]struct{}{}
	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, err
		}
		logical, _, _ := entryPath(hdr.Name)
		if pol.Excluded(logical) {
			st.Dropped++
			continue
		}
		// A name that looks like a whiteout is a deletion marker to every unpacker. Only containerd's own synthetic markers are kept: a
		// real file, directory or link a participant named ".wh.x" would delete a lower file or break the layer on restore.
		if _, whiteout, opaque := entryPath(hdr.Name); (whiteout || opaque) && !syntheticWhiteout(hdr) {
			st.RefusedEntries++
			st.Dropped++
			continue
		}
		// Device nodes and named pipes are never kept: a snapshot restored on a node must not create them (the device cgroup
		// would stop a device node working, but nothing in a snapshot has a reason to carry one).
		if hdr.Typeflag == tar.TypeChar || hdr.Typeflag == tar.TypeBlock || hdr.Typeflag == tar.TypeFifo {
			st.RefusedEntries++
			st.Dropped++
			continue
		}
		if headerBytes(hdr) > MaxEntryHeaderBytes {
			st.RefusedEntries++
			st.Dropped++
			continue
		}
		if pol.MaxFileSize > 0 && hdr.Typeflag == tar.TypeReg && hdr.Size > pol.MaxFileSize {
			st.skip(logical, hdr.Size)
			continue
		}
		// The apparent size of a file is in its header, and a sparse file streams all its zeros: stop at the quota without reading them.
		if pol.WriteQuota > 0 && hdr.Typeflag == tar.TypeReg && st.Bytes+hdr.Size > pol.WriteQuota {
			return st, fmt.Errorf("%w: the layer passes the quota of %d bytes", ErrQuota, pol.WriteQuota)
		}
		if hdr.Typeflag == tar.TypeLink {
			if _, ok := emitted[CleanPath(hdr.Linkname)]; !ok {
				st.Dropped++ // its target was not written: the link would dangle
				continue
			}
		}
		if pol.MaxEntries > 0 && st.Entries >= pol.MaxEntries {
			return st, fmt.Errorf("%w: more than %d entries", ErrEntries, pol.MaxEntries)
		}
		if len(ids.UID) > 0 || len(ids.GID) > 0 {
			if !syntheticWhiteout(hdr) {
				var n int
				hdr.Uid, hdr.Gid, n = ids.translate(hdr.Uid, hdr.Gid)
				st.Unmapped += n
			}
			delete(hdr.PAXRecords, "uid")
			delete(hdr.PAXRecords, "gid")
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return st, err
		}
		if hdr.Typeflag == tar.TypeReg {
			n, err := io.Copy(tw, tr)
			if err != nil {
				return st, err
			}
			st.Bytes += n
		}
		emitted[CleanPath(hdr.Name)] = struct{}{}
		st.Entries++
	}
	return st, tw.Close()
}

// syntheticWhiteout recognizes the empty root-owned deletion headers generated
// by containerd's ChangeWriter. They do not describe an inode's host ownership.
// Payloads, links, named owners, xattrs and nonstandard metadata retain the normal
// owner checks, even when their filename resembles a whiteout.
func syntheticWhiteout(h *tar.Header) bool {
	if h.Typeflag != tar.TypeReg || h.Size != 0 || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || h.Linkname != "" || h.Devmajor != 0 || h.Devminor != 0 {
		return false
	}
	for k := range h.PAXRecords {
		switch k {
		case "path", "uid", "gid", "mtime", "atime", "ctime": // ordinary tar serialization of the synthetic header
		default:
			return false
		}
	}
	base := path.Base(h.Name)
	if base == whiteoutOpaque {
		return true
	}
	deleted, ok := strings.CutPrefix(base, whiteoutPrefix)
	return ok && deleted != "" && deleted != "." && deleted != ".." && !strings.HasPrefix(deleted, whiteoutPrefix)
}

// headerBytes is what the names and extended attributes of an entry take.
func headerBytes(h *tar.Header) int {
	n := len(h.Name) + len(h.Linkname) + len(h.Uname) + len(h.Gname)
	for k, v := range h.PAXRecords {
		n += len(k) + len(v)
	}
	for k, v := range h.Xattrs { //nolint:staticcheck // the deprecated field still carries xattrs of older writers
		n += len(k) + len(v)
	}
	return n
}

func (s *Stats) skip(path string, size int64) {
	s.Dropped++
	s.SkippedTotal++
	if len(s.Skipped) < MaxSkippedListed {
		s.Skipped = append(s.Skipped, SkippedFile{Path: path, Size: size})
	}
}
