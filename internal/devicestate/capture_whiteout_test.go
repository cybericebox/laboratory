package devicestate

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

func captureHeaderTar(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func remappedCaptureRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, time.Hour, 1<<20)
	m := snapshot.IDMap{{ContainerID: 0, HostID: 3686989824, Size: 65536}}
	r.e.Runtime = &metadataRuntime{fakeRuntime: r.rt, known: true, ids: snapshot.IDMaps{UID: m, GID: m}}
	return r
}

func TestRequiredCaptureAcceptsSyntheticWhiteoutsWithMappedData(t *testing.T) {
	r := remappedCaptureRig(t)
	req := captureRequest(r)
	r.rt.setDiff(captureHeaderTar(t,
		&tar.Header{Name: "etc/.wh.alpine-release", Typeflag: tar.TypeReg},
		&tar.Header{Name: "data/.wh..wh..opq", Typeflag: tar.TypeReg},
		&tar.Header{Name: "data/work", Typeflag: tar.TypeReg, Size: 1, Uid: 3686989947, Gid: 3686990280},
	))
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err != nil || result.Result != "Succeeded" || result.Image == "" || result.SizeBytes != 1 || !result.Quiesced || result.GuardState != "Held" {
		t.Fatalf("required whiteout capture: %+v %v", result, err)
	}
	img, err := r.reg.Image(context.Background(), r.pod.Repo)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := layers[len(layers)-1].Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for i, name := range []string{"etc/.wh.alpine-release", "data/.wh..wh..opq", "data/work"} {
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		uid, gid, size := 0, 0, int64(0)
		if i == 2 {
			uid, gid, size = 123, 456, 1
		}
		if h.Name != name || h.Typeflag != tar.TypeReg || h.Uid != uid || h.Gid != gid || h.Size != size || h.PAXRecords["uid"] != "" || h.PAXRecords["gid"] != "" {
			t.Fatalf("saved entry changed: %+v", h)
		}
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("unexpected entry: %v", err)
	}
}

func TestRequiredCaptureRejectsUnmappedOwnersAndWhiteoutLookalikes(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    tar.Header
	}{
		{"regular", tar.Header{Name: "data", Typeflag: tar.TypeReg}},
		{"hardlink", tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "data"}},
		{"symlink", tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "data"}},
		{"payload", tar.Header{Name: ".wh.data", Typeflag: tar.TypeReg, Size: 1}},
		{"owner", tar.Header{Name: ".wh.data", Typeflag: tar.TypeReg, Uid: 42}},
		{"owner_name", tar.Header{Name: ".wh.data", Typeflag: tar.TypeReg, Uname: "other"}},
		{"xattr", tar.Header{Name: ".wh.data", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"SCHILY.xattr.user.test": "data"}}},
		{"parent_basename", tar.Header{Name: ".wh...", Typeflag: tar.TypeReg}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := remappedCaptureRig(t)
			req := captureRequest(r)
			r.rt.setDiff(captureHeaderTar(t, &tc.h))
			result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
			if err == nil || !strings.Contains(err.Error(), "omits file data or ownership") || result.Result != "Failed" || result.Image != "" || result.Quiesced {
				t.Fatalf("unmapped ownership acknowledged: %+v %v", result, err)
			}
			records, _, exits := r.cl.snapshot()
			if len(records) != 0 || len(exits) != 0 {
				t.Fatal("failure changed latest snapshot or exit acknowledgement")
			}
		})
	}
}
