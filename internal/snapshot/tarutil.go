package snapshot

import (
	"archive/tar"
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
}

// FilterLayer copies an uncompressed layer tar from in to out, leaving out the
// entries (and whiteouts) of excluded paths. The result is a complete tar.
func FilterLayer(in io.Reader, out io.Writer, pol Policy) (Stats, error) {
	var st Stats
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
