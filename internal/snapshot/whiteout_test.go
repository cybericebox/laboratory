package snapshot

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/continuity/fs"
)

const whiteoutHostID = 3686989824

func whiteoutIDs() IDMaps {
	m := IDMap{{ContainerID: 0, HostID: whiteoutHostID, Size: 65536}}
	return IDMaps{UID: m, GID: m}
}

func headerTar(t *testing.T, headers ...*tar.Header) []byte {
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

// containerd's deletion writer synthesizes root-owned markers rather than using
// the shifted ownership of a file in the namespace being captured.
func TestFilterLayerMappedContainerdWhiteout(t *testing.T) {
	var in, out bytes.Buffer
	cw := archive.NewChangeWriter(&in, t.TempDir())
	if err := cw.HandleChange(fs.ChangeKindDelete, "/alpine-release", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := FilterLayerMapped(&in, &out, NewPolicy(0, nil, 0, 0), whiteoutIDs())
	if err != nil {
		t.Fatal(err)
	}
	if st.Unmapped != 0 || st.Entries != 1 || st.Bytes != 0 || st.Dropped != 0 {
		t.Fatalf("synthetic whiteout lost ownership: %+v", st)
	}
	h, err := tar.NewReader(&out).Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != ".wh.alpine-release" || h.Typeflag != tar.TypeReg || h.Size != 0 || h.Uid != 0 || h.Gid != 0 || h.PAXRecords["uid"] != "" || h.PAXRecords["gid"] != "" {
		t.Fatalf("whiteout changed: %+v", h)
	}
}

func TestFilterLayerMappedWhiteoutsPreserveDeletionAndMetadata(t *testing.T) {
	in := headerTar(t,
		&tar.Header{Name: "etc/.wh.alpine-release", Typeflag: tar.TypeReg},
		&tar.Header{Name: "data/.wh..wh..opq", Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), AccessTime: time.Unix(0, 0), ChangeTime: time.Unix(0, 0)},
		&tar.Header{Name: "data/work", Typeflag: tar.TypeReg, Size: 1, Uid: whiteoutHostID + 123, Gid: whiteoutHostID + 456, PAXRecords: map[string]string{"SCHILY.xattr.user.test": "kept"}},
		&tar.Header{Name: "data/link", Typeflag: tar.TypeLink, Linkname: "data/work", Uid: whiteoutHostID + 123, Gid: whiteoutHostID + 456},
		&tar.Header{Name: "data/symlink", Typeflag: tar.TypeSymlink, Linkname: "work", Uid: whiteoutHostID + 123, Gid: whiteoutHostID + 456},
	)
	var filtered bytes.Buffer
	st, err := FilterLayerMapped(bytes.NewReader(in), &filtered, NewPolicy(0, nil, 0, 0), whiteoutIDs())
	if err != nil || st.Unmapped != 0 || st.Entries != 5 || st.Bytes != 1 || st.Dropped != 0 {
		t.Fatalf("mapped layer: %+v %v", st, err)
	}
	tr := tar.NewReader(bytes.NewReader(filtered.Bytes()))
	for i := 0; i < 5; i++ {
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && (h.Uid != 0 || h.Gid != 0 || h.Typeflag != tar.TypeReg || h.Size != 0) {
			t.Fatalf("marker metadata: %+v", h)
		}
		if i >= 2 && (h.Uid != 123 || h.Gid != 456 || h.PAXRecords["uid"] != "" || h.PAXRecords["gid"] != "") {
			t.Fatalf("ordinary entry ownership: %+v", h)
		}
		if i == 2 && h.PAXRecords["SCHILY.xattr.user.test"] != "kept" {
			t.Fatalf("file xattr lost: %+v", h)
		}
		if i == 3 && (h.Typeflag != tar.TypeLink || h.Linkname != "data/work") || i == 4 && (h.Typeflag != tar.TypeSymlink || h.Linkname != "work") {
			t.Fatalf("link changed: %+v", h)
		}
	}
	older := mkTar(t, ent{name: "etc/alpine-release", body: "old"}, ent{name: "data/old", body: "old"})
	var merged bytes.Buffer
	if _, err := MergeLayers([]Opener{opener(older), opener(filtered.Bytes())}, &merged); err != nil {
		t.Fatal(err)
	}
	files, markers, _ := mergedView(t, merged.Bytes())
	if !reflect.DeepEqual(files, map[string]string{"data/work": "x", "data/link": "", "data/symlink": ""}) || !reflect.DeepEqual(markers, []string{"data/.wh..wh..opq", "etc/.wh.alpine-release"}) {
		t.Fatalf("deletion semantics changed: files=%v markers=%v", files, markers)
	}
}

func TestFilterLayerMappedWhiteoutLookalikesRetainUnmappedOwners(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    tar.Header
	}{
		{"regular", tar.Header{Name: "work", Typeflag: tar.TypeReg}},
		{"hardlink", tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "work"}},
		{"symlink", tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "work"}},
		{"whiteout_payload", tar.Header{Name: ".wh.work", Typeflag: tar.TypeReg, Size: 1}},
		{"whiteout_hardlink", tar.Header{Name: ".wh.work", Typeflag: tar.TypeLink, Linkname: "work"}},
		{"whiteout_symlink", tar.Header{Name: ".wh.work", Typeflag: tar.TypeSymlink, Linkname: "work"}},
		{"whiteout_directory", tar.Header{Name: ".wh.work/", Typeflag: tar.TypeDir}},
		{"whiteout_linkname", tar.Header{Name: ".wh.work", Typeflag: tar.TypeReg, Linkname: "work"}},
		{"whiteout_owner", tar.Header{Name: ".wh.work", Typeflag: tar.TypeReg, Uid: 42}},
		{"whiteout_pax_owner", tar.Header{Name: ".wh.work", Typeflag: tar.TypeReg, Uid: whiteoutHostID + 65536, Format: tar.FormatPAX}},
		{"whiteout_owner_name", tar.Header{Name: ".wh.work", Typeflag: tar.TypeReg, Uname: "other"}},
		{"whiteout_group_name", tar.Header{Name: ".wh.work", Typeflag: tar.TypeReg, Gname: "other"}},
		{"whiteout_xattr", tar.Header{Name: ".wh.work", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"SCHILY.xattr.user.test": "data"}}},
		{"whiteout_unknown_pax", tar.Header{Name: ".wh.work", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"custom": "data"}}},
		{"empty_basename", tar.Header{Name: ".wh.", Typeflag: tar.TypeReg}},
		{"dot_basename", tar.Header{Name: ".wh..", Typeflag: tar.TypeReg}},
		{"parent_basename", tar.Header{Name: ".wh...", Typeflag: tar.TypeReg}},
		{"reserved_basename", tar.Header{Name: ".wh..wh.other", Typeflag: tar.TypeReg}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			st, err := FilterLayerMapped(bytes.NewReader(headerTar(t, &tc.h)), &out, NewPolicy(0, nil, 0, 0), whiteoutIDs())
			if err != nil {
				t.Fatal(err)
			}
			if st.Unmapped != 2 || st.Entries != 1 {
				t.Fatalf("unmapped lookalike bypassed owner checks: %+v", st)
			}
		})
	}
}

func TestFilterLayerMappedWhiteoutRemovesPAXOwnership(t *testing.T) {
	in := headerTar(t, &tar.Header{Name: ".wh.work", Typeflag: tar.TypeReg, Format: tar.FormatPAX, PAXRecords: map[string]string{"uid": "0", "gid": "0"}})
	h, err := tar.NewReader(bytes.NewReader(in)).Next()
	if err != nil || h.PAXRecords["uid"] != "0" || h.PAXRecords["gid"] != "0" {
		t.Fatalf("PAX ownership fixture missing: %+v %v", h, err)
	}
	var out bytes.Buffer
	st, err := FilterLayerMapped(bytes.NewReader(in), &out, NewPolicy(0, nil, 0, 0), whiteoutIDs())
	if err != nil || st.Unmapped != 0 {
		t.Fatalf("synthetic PAX ownership: %+v %v", st, err)
	}
	h, err = tar.NewReader(&out).Next()
	if err != nil || h.Uid != 0 || h.Gid != 0 || h.PAXRecords["uid"] != "" || h.PAXRecords["gid"] != "" {
		t.Fatalf("PAX ownership retained: %+v %v", h, err)
	}
}

func TestFilterLayerMappedWhiteoutsKeepStrictBudgets(t *testing.T) {
	for _, tc := range []struct {
		name string
		pol  Policy
		in   []byte
		want error
	}{
		{"entries", NewPolicy(0, nil, 0, 0).WithMaxEntries(1), headerTar(t, &tar.Header{Name: ".wh.a", Typeflag: tar.TypeReg}, &tar.Header{Name: ".wh.b", Typeflag: tar.TypeReg}), ErrEntries},
		{"quota", NewPolicy(0, nil, 1, 0), headerTar(t, &tar.Header{Name: ".wh.a", Typeflag: tar.TypeReg}, &tar.Header{Name: "data", Typeflag: tar.TypeReg, Size: 2, Uid: whiteoutHostID, Gid: whiteoutHostID}), ErrQuota},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := FilterLayerMapped(bytes.NewReader(tc.in), &out, tc.pol, whiteoutIDs())
			if !errors.Is(err, tc.want) {
				t.Fatalf("budget bypassed: %v", err)
			}
		})
	}
	var out bytes.Buffer
	st, err := FilterLayerMapped(bytes.NewReader(headerTar(t, &tar.Header{Name: ".wh.a", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"large": strings.Repeat("x", MaxEntryHeaderBytes+1)}})), &out, NewPolicy(0, nil, 0, 0), whiteoutIDs())
	if err != nil || st.RefusedEntries != 1 || st.Dropped != 1 || st.Entries != 0 {
		t.Fatalf("header budget bypassed: %+v %v", st, err)
	}
	if _, err := tar.NewReader(&out).Next(); err != io.EOF {
		t.Fatalf("refused header retained: %v", err)
	}
}
