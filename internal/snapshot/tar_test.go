package snapshot

import (
	"archive/tar"
	"bytes"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type ent struct {
	name string
	body string // "" with dir=true for directories
	dir  bool
	link string
}

func mkTar(t *testing.T, ents ...ent) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(e.body))}
		switch {
		case e.dir:
			h.Typeflag, h.Size, h.Mode = tar.TypeDir, 0, 0o755
		case e.link != "":
			h.Typeflag, h.Size, h.Linkname = tar.TypeLink, 0, e.link
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func names(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

func opener(b []byte) Opener {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

func TestPolicyDefaultsAndExclusion(t *testing.T) {
	p := NewPolicy(0, nil, 0, 0)
	if p.Debounce != DefaultDebounce || p.WriteQuota != DefaultWriteQuota || p.MaxLayers != DefaultMaxLayers {
		t.Fatalf("defaults not applied: %+v", p)
	}
	for _, path := range []string{"/tmp", "/tmp/x/y", "tmp/a", "/var/tmp/f", "/run/lock", "/proc/1", "/etc/hosts", "/var/run/secrets/kubernetes.io/serviceaccount/token"} {
		if !p.Excluded(path) {
			t.Errorf("%s should be excluded", path)
		}
	}
	for _, path := range []string{"/tmpfoo", "/var/www/index.html", "/etc/hostsfile", "/home/user/.bashrc", "/var"} {
		if p.Excluded(path) {
			t.Errorf("%s should be kept", path)
		}
	}
	// An operator list replaces the default one but never drops the system paths.
	q := NewPolicy(0, []string{"/cache/"}, 0, 0)
	if !q.Excluded("/cache/a") || q.Excluded("/tmp/a") || !q.Excluded("/dev/null") {
		t.Fatalf("custom list mishandled: %v", q.ExcludePaths)
	}
}

func TestQuotaAndSquashRules(t *testing.T) {
	if err := CheckQuota(100, 50, 200); err != nil {
		t.Fatal(err)
	}
	if err := CheckQuota(100, 101, 200); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("want quota error, got %v", err)
	}
	if err := CheckQuota(1<<40, 1<<40, 0); err != nil {
		t.Fatal("zero max means unlimited")
	}
	if NeedSquash(9, 10) || !NeedSquash(10, 10) || NeedSquash(0, 0) {
		t.Fatal("squash threshold wrong")
	}
}

func TestRepoNames(t *testing.T) {
	if got := Repo("ns-1", "lab-a", "web"); got != "lab/ns-1/lab-a/web" {
		t.Fatal(got)
	}
	if !strings.HasPrefix(Repo("ns-1", "lab-a", "web"), LabRepoPrefix("ns-1", "lab-a")) {
		t.Fatal("device repo must sit under its lab prefix")
	}
	if strings.HasPrefix(Repo("ns-1", "lab-ab", "web"), LabRepoPrefix("ns-1", "lab-a")) {
		t.Fatal("lab prefix must not match a longer lab name")
	}
}

func TestFilterLayerDropsExcluded(t *testing.T) {
	in := mkTar(t,
		ent{name: "etc/app.conf", body: "x=1"},
		ent{name: "tmp/scratch", body: "junk"},
		ent{name: "tmp/.wh.old", body: ""},
		ent{name: "var/tmp", dir: true},
		ent{name: "run/lock/x", body: "l"},
		ent{name: "home/u/.wh..wh..opq", body: ""},
		ent{name: "etc/hosts", body: ""},
		ent{name: "srv/data.db", body: "abcd"},
	)
	var out bytes.Buffer
	st, err := FilterLayer(bytes.NewReader(in), &out, NewPolicy(0, nil, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := names(t, out.Bytes())
	want := []string{"etc/app.conf", "home/u/.wh..wh..opq", "srv/data.db"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if st.Entries != 3 || st.Bytes != 7 || st.Dropped != 5 {
		t.Fatalf("stats %+v", st)
	}
}

// view applies merged tar semantics for assertions: path -> body, with
// markers listed separately.
func mergedView(t *testing.T, b []byte) (files map[string]string, markers []string, order []string) {
	t.Helper()
	files = map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			sort.Strings(markers)
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, h.Name)
		_, wh, opq := entryPath(h.Name)
		if wh || opq {
			markers = append(markers, h.Name)
			continue
		}
		body, _ := io.ReadAll(tr)
		files[h.Name] = string(body)
	}
}

func TestMergeLayersNewestWinsAndDeletes(t *testing.T) {
	l1 := mkTar(t,
		ent{name: "app", dir: true},
		ent{name: "app/a.txt", body: "one"},
		ent{name: "app/b.txt", body: "bee"},
		ent{name: "data/x", body: "x1"},
	)
	l2 := mkTar(t,
		ent{name: "app/a.txt", body: "two"},
		ent{name: "app/.wh.b.txt"},
		ent{name: "data/.wh..wh..opq"},
		ent{name: "data/y", body: "y2"},
	)
	l3 := mkTar(t,
		ent{name: "app/a.txt", body: "three"},
		ent{name: "base/.wh.lib"},
	)
	var out bytes.Buffer
	st, err := MergeLayers([]Opener{opener(l1), opener(l2), opener(l3)}, &out)
	if err != nil {
		t.Fatal(err)
	}
	files, markers, order := mergedView(t, out.Bytes())

	wantFiles := map[string]string{"app": "", "app/a.txt": "three", "data/y": "y2"}
	if !reflect.DeepEqual(files, wantFiles) {
		t.Fatalf("files %v want %v", files, wantFiles)
	}
	// b.txt was deleted in l2 but still exists in lower layers of the image,
	// so its whiteout stays; data/x is hidden by the opaque marker.
	wantMarkers := []string{"app/.wh.b.txt", "base/.wh.lib", "data/.wh..wh..opq"}
	if !reflect.DeepEqual(markers, wantMarkers) {
		t.Fatalf("markers %v want %v", markers, wantMarkers)
	}
	for i, n := range order {
		if _, wh, opq := entryPath(n); !wh && !opq && i < len(markers) {
			t.Fatalf("markers must come first, got %v", order)
		}
	}
	if st.Entries != len(order) {
		t.Fatalf("stats %+v order %v", st, order)
	}
}

func TestMergeLayersRecreateAfterDelete(t *testing.T) {
	l1 := mkTar(t, ent{name: "f", body: "old"})
	l2 := mkTar(t, ent{name: ".wh.f"})
	l3 := mkTar(t, ent{name: "f", body: "new"})
	var out bytes.Buffer
	if _, err := MergeLayers([]Opener{opener(l1), opener(l2), opener(l3)}, &out); err != nil {
		t.Fatal(err)
	}
	files, markers, _ := mergedView(t, out.Bytes())
	// The new file is kept and the whiteout stays to hide the base image's f.
	if files["f"] != "new" || !reflect.DeepEqual(markers, []string{".wh.f"}) {
		t.Fatalf("files %v markers %v", files, markers)
	}
}

func TestMergeLayersCreatedThenDeletedVanishes(t *testing.T) {
	l1 := mkTar(t, ent{name: "tmpfile", body: "x"}, ent{name: "dir", dir: true}, ent{name: "dir/k", body: "k"})
	l2 := mkTar(t, ent{name: ".wh.tmpfile"}, ent{name: ".wh.dir"})
	var out bytes.Buffer
	if _, err := MergeLayers([]Opener{opener(l1), opener(l2)}, &out); err != nil {
		t.Fatal(err)
	}
	files, markers, _ := mergedView(t, out.Bytes())
	if len(files) != 0 {
		t.Fatalf("deleted paths must not survive: %v", files)
	}
	if !reflect.DeepEqual(markers, []string{".wh.dir", ".wh.tmpfile"}) {
		t.Fatalf("markers %v", markers)
	}
}

func TestMergeLayersFileReplacingDirectoryHidesChildren(t *testing.T) {
	l1 := mkTar(t, ent{name: "d", dir: true}, ent{name: "d/child", body: "c"})
	l2 := mkTar(t, ent{name: "d", body: "now a file"})
	var out bytes.Buffer
	if _, err := MergeLayers([]Opener{opener(l1), opener(l2)}, &out); err != nil {
		t.Fatal(err)
	}
	files, _, _ := mergedView(t, out.Bytes())
	if !reflect.DeepEqual(files, map[string]string{"d": "now a file"}) {
		t.Fatalf("got %v", files)
	}
}

func TestMergeLayersOrderKeepsParentsFirst(t *testing.T) {
	l1 := mkTar(t, ent{name: "a", dir: true}, ent{name: "a/b", dir: true})
	l2 := mkTar(t, ent{name: "a/b/c", body: "c"})
	var out bytes.Buffer
	if _, err := MergeLayers([]Opener{opener(l1), opener(l2)}, &out); err != nil {
		t.Fatal(err)
	}
	_, _, order := mergedView(t, out.Bytes())
	if !reflect.DeepEqual(order, []string{"a", "a/b", "a/b/c"}) {
		t.Fatalf("order %v", order)
	}
}

func TestMergeLayersDropsDanglingHardlink(t *testing.T) {
	l1 := mkTar(t, ent{name: "target", body: "t"}, ent{name: "link", link: "target"})
	l2 := mkTar(t, ent{name: ".wh.target"})
	var out bytes.Buffer
	if _, err := MergeLayers([]Opener{opener(l1), opener(l2)}, &out); err != nil {
		t.Fatal(err)
	}
	files, _, _ := mergedView(t, out.Bytes())
	if _, ok := files["link"]; ok {
		t.Fatalf("link to a deleted target must be dropped: %v", files)
	}
}

func TestFilterLayerSkipsFilesOverTheSizeLimit(t *testing.T) {
	in := mkTar(t,
		ent{name: "data/", dir: true},
		ent{name: "data/big.bin", body: strings.Repeat("x", 100)},
		ent{name: "data/small.txt", body: "ok"},
		ent{name: "data/link.bin", link: "data/big.bin"},
		ent{name: "data/.wh.deleted"},
		ent{name: "data/exact", body: strings.Repeat("y", 50)},
	)
	pol := NewPolicy(0, nil, 0, 0).WithMaxFileSize(50)
	var out bytes.Buffer
	st, err := FilterLayer(bytes.NewReader(in), &out, pol)
	if err != nil {
		t.Fatal(err)
	}
	got := names(t, out.Bytes())
	want := []string{"data/", "data/small.txt", "data/.wh.deleted", "data/exact"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v (the big file and the hard link to it are skipped; whiteouts and a file at the limit stay)", got, want)
	}
	if st.SkippedTotal != 1 || len(st.Skipped) != 1 || st.Skipped[0] != (SkippedFile{Path: "/data/big.bin", Size: 100}) {
		t.Fatalf("skipped %+v", st)
	}
	if st.Bytes != 52 {
		t.Fatalf("only the kept files count: %d", st.Bytes)
	}
}

func TestSkippedWarningNamesAtMostTenFiles(t *testing.T) {
	var st Stats
	if st.SkippedWarning(10) != "" {
		t.Fatal("nothing skipped, no warning")
	}
	for i := 0; i < 13; i++ {
		st.skip("/f"+string(rune('a'+i)), int64(100+i))
	}
	w := st.SkippedWarning(64)
	if len(st.Skipped) != MaxSkippedListed || st.SkippedTotal != 13 ||
		!strings.Contains(w, "files over 64 bytes") || !strings.Contains(w, "/fa (100 bytes)") || strings.Contains(w, "/fk") || !strings.HasSuffix(w, "and 3 more") {
		t.Fatalf("warning %q", w)
	}
}

func TestPolicyMaxFileSizeDefault(t *testing.T) {
	if p := NewPolicy(0, nil, 0, 0); p.MaxFileSize != DefaultMaxFileSize || p.WithMaxFileSize(0).MaxFileSize != DefaultMaxFileSize || p.WithMaxFileSize(7).MaxFileSize != 7 {
		t.Fatalf("%+v", p)
	}
}
