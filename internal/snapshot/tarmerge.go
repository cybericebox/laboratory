package snapshot

import (
	"archive/tar"
	"fmt"
	"io"
	"strings"
)

// Opener returns a fresh reader of an uncompressed layer tar.
type Opener func() (io.ReadCloser, error)

// markerSet is what newer layers hide from older ones.
type markerSet struct {
	whiteouts map[string]bool // path deleted: the path itself and everything under it
	opaque    map[string]bool // directory emptied: everything strictly under it
	replaced  map[string]bool // non-directory that replaced a path: everything strictly under it
}

func newMarkerSet() *markerSet {
	return &markerSet{whiteouts: map[string]bool{}, opaque: map[string]bool{}, replaced: map[string]bool{}}
}

// hides reports whether a path from an older layer is gone because of this set.
func (m *markerSet) hides(p string) bool {
	if m.whiteouts[p] {
		return true
	}
	for a := parentOf(p); a != ""; a = parentOf(a) {
		if m.whiteouts[a] || m.opaque[a] || m.replaced[a] {
			return true
		}
	}
	return false
}

// parentOf returns the parent of an absolute path, or "" for the root.
func parentOf(p string) string {
	if p == "/" || p == "" {
		return ""
	}
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// MergeLayers squashes uncompressed layer tars (oldest first) into one tar,
// keeping the effect of applying them in order on top of any lower layer:
// the newest version of every path wins, deleted paths disappear, and the
// whiteout and opaque markers that still have to hide lower layers are kept.
// Markers are written first, so an unpacker that removes on a whiteout never
// removes a file of the same layer.
func MergeLayers(layers []Opener, out io.Writer) (Stats, error) {
	var st Stats
	type keep map[int]bool

	kept := make([]keep, len(layers))
	emitted := map[string]bool{}
	hidden := newMarkerSet()
	var markers []*tar.Header
	seenMarker := map[string]bool{}

	// Pass 1, newest layer first: decide which entries survive.
	for i := len(layers) - 1; i >= 0; i-- {
		rc, err := layers[i]()
		if err != nil {
			return st, err
		}
		kept[i] = keep{}
		layerMarkers := newMarkerSet()
		tr := tar.NewReader(rc)
		for idx := 0; ; idx++ {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				rc.Close()
				return st, fmt.Errorf("layer %d: %w", i, err)
			}
			logical, whiteout, opaque := entryPath(hdr.Name)
			switch {
			case whiteout:
				if hidden.hides(logical) || seenMarker["w"+logical] {
					st.Dropped++
					continue
				}
				seenMarker["w"+logical] = true
				markers = append(markers, cloneHeader(hdr))
				layerMarkers.whiteouts[logical] = true
			case opaque:
				if hidden.hides(logical) || seenMarker["o"+logical] {
					st.Dropped++
					continue
				}
				seenMarker["o"+logical] = true
				markers = append(markers, cloneHeader(hdr))
				layerMarkers.opaque[logical] = true
			default:
				if hidden.hides(logical) || emitted[logical] {
					st.Dropped++
					continue
				}
				emitted[logical] = true
				kept[i][idx] = true
				if hdr.Typeflag != tar.TypeDir {
					layerMarkers.replaced[logical] = true
				}
			}
		}
		rc.Close()
		for k := range layerMarkers.whiteouts {
			hidden.whiteouts[k] = true
		}
		for k := range layerMarkers.opaque {
			hidden.opaque[k] = true
		}
		for k := range layerMarkers.replaced {
			hidden.replaced[k] = true
		}
	}

	// Pass 2, oldest layer first: write markers, then the surviving entries.
	tw := tar.NewWriter(out)
	for _, h := range markers {
		if err := tw.WriteHeader(h); err != nil {
			return st, err
		}
		st.Entries++
	}
	for i := range layers {
		rc, err := layers[i]()
		if err != nil {
			return st, err
		}
		tr := tar.NewReader(rc)
		for idx := 0; ; idx++ {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				rc.Close()
				return st, fmt.Errorf("layer %d: %w", i, err)
			}
			if !kept[i][idx] {
				continue
			}
			if hdr.Typeflag == tar.TypeLink && !emitted[CleanPath(hdr.Linkname)] {
				st.Dropped++
				continue
			}
			if err := tw.WriteHeader(hdr); err != nil {
				rc.Close()
				return st, err
			}
			if hdr.Typeflag == tar.TypeReg {
				n, err := io.Copy(tw, tr)
				if err != nil {
					rc.Close()
					return st, err
				}
				st.Bytes += n
			}
			st.Entries++
		}
		rc.Close()
	}
	return st, tw.Close()
}

func cloneHeader(h *tar.Header) *tar.Header {
	c := *h
	c.Size = 0
	return &c
}
