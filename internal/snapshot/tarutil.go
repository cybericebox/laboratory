package snapshot

import (
	"archive/tar"
	"fmt"
	"io"
	"path"
	"strings"
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
	// Skipped are the first MaxSkippedListed regular files left out for being larger than
	// the policy's MaxFileSize; SkippedTotal counts all of them.
	Skipped      []SkippedFile
	SkippedTotal int
}

// SkippedFile is a regular file left out of a snapshot for its size.
type SkippedFile struct {
	Path string
	Size int64
}

// MaxSkippedListed caps the skipped files named in a warning.
const MaxSkippedListed = 10

// SkippedWarning is the status warning for the files skipped for size; empty when none.
func (s Stats) SkippedWarning(maxFileSize int64) string {
	if s.SkippedTotal == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.Skipped))
	for _, f := range s.Skipped {
		parts = append(parts, fmt.Sprintf("%s (%d bytes)", f.Path, f.Size))
	}
	msg := fmt.Sprintf("files over %d bytes are not kept: %s", maxFileSize, strings.Join(parts, ", "))
	if more := s.SkippedTotal - len(s.Skipped); more > 0 {
		msg += fmt.Sprintf(" and %d more", more)
	}
	return msg
}

// FilterLayer copies an uncompressed layer tar from in to out, leaving out the
// entries (and whiteouts) of excluded paths and the regular files larger than the
// policy's MaxFileSize (with the hard links to them). Whiteouts are empty files and
// never skipped for size. The result is a complete tar.
func FilterLayer(in io.Reader, out io.Writer, pol Policy) (Stats, error) {
	var st Stats
	skipped := map[string]bool{}
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
		// Device nodes and named pipes are never kept: a snapshot restored on a node must not create them (the device cgroup
		// would stop a device node working, but nothing in a snapshot has a reason to carry one).
		if hdr.Typeflag == tar.TypeChar || hdr.Typeflag == tar.TypeBlock || hdr.Typeflag == tar.TypeFifo {
			st.Dropped++
			continue
		}
		if headerBytes(hdr) > MaxEntryHeaderBytes {
			st.Dropped++
			continue
		}
		if pol.MaxFileSize > 0 && hdr.Typeflag == tar.TypeReg && hdr.Size > pol.MaxFileSize {
			st.skip(logical, hdr.Size)
			skipped[logical] = true
			continue
		}
		// The apparent size of a file is in its header, and a sparse file streams all its zeros: stop at the quota without reading them.
		if pol.WriteQuota > 0 && hdr.Typeflag == tar.TypeReg && st.Bytes+hdr.Size > pol.WriteQuota {
			return st, fmt.Errorf("%w: the layer passes the quota of %d bytes", ErrQuota, pol.WriteQuota)
		}
		if hdr.Typeflag == tar.TypeLink && skipped[CleanPath(hdr.Linkname)] {
			st.Dropped++ // a hard link to a skipped file would dangle
			continue
		}
		if pol.MaxEntries > 0 && st.Entries >= pol.MaxEntries {
			return st, fmt.Errorf("%w: more than %d entries", ErrEntries, pol.MaxEntries)
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
		st.Entries++
	}
	return st, tw.Close()
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
