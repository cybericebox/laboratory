package snapshot

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"
)

// A sparse file counts by its apparent size, which is in its header: the layer is refused before the zeros are read.
func TestFilterLayerStopsAtTheQuotaByApparentSize(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "huge", Typeflag: tar.TypeReg, Size: 1 << 40, Mode: 0o644})
	// no body follows: reading it would fail, and the filter must not try
	pol := NewPolicy(0, nil, 1<<20, 0)
	pol.MaxFileSize = 1 << 50
	_, err := FilterLayer(&buf, io.Discard, pol)
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("want ErrQuota, got %v", err)
	}
}
