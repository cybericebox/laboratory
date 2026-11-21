package devicestate

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

func tarEntries(t *testing.T, b []byte) map[string]*tar.Header {
	t.Helper()
	out := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

func (f *userXattrFixture) enrichedReport(t *testing.T, raw []byte, pol snapshot.Policy) ([]byte, xattrReport, error) {
	t.Helper()
	var out bytes.Buffer
	var rep xattrReport
	err := writeUserXattrLayerReport(context.Background(), bytes.NewReader(raw), &out, f.up, f.live, f.lo, pol, &rep)
	return out.Bytes(), rep, err
}

// A hard link whose target is an excluded path would dangle: this stage drops it. A link whose target it passed on stays.
func TestLayerUserXattrsDropsAHardLinkToAnExcludedTarget(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "ex/target", "", "payload")
	f.file(t, "keep/ok", "", "kept")
	for _, root := range []string{f.merged, f.upper} {
		// the walk order puts "ex/target" first, so containerd writes "zz/link" and "zz/okl" as links to what came before
		if err := os.MkdirAll(filepath.Join(root, "zz"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, "ex/target"), filepath.Join(root, "zz/link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, "keep/ok"), filepath.Join(root, "zz/okl")); err != nil {
			t.Fatal(err)
		}
	}
	raw := f.raw(t)
	if !tarHasPath(t, raw, "zz/link") || !tarHasPath(t, raw, "zz/okl") {
		t.Fatal("fixture: containerd did not write the two hard links")
	}
	pol := snapshot.NewPolicy(0, []string{"/ex/"}, 0, 0)
	out, rep, err := f.enrichedReport(t, raw, pol)
	if err != nil {
		t.Fatal(err)
	}
	got := tarEntries(t, out)
	if _, ok := got["zz/link"]; ok {
		t.Fatal("a hard link to an excluded target was passed on: it dangles")
	}
	if _, ok := got["ex/target"]; ok {
		t.Fatal("the excluded target was passed on")
	}
	if h := got["zz/okl"]; h == nil || h.Typeflag != tar.TypeLink || h.Linkname != "keep/ok" {
		t.Fatalf("the link to a kept target must stay: %+v", h)
	}
	if rep.DroppedLinks != 1 {
		t.Fatalf("report: %+v", rep)
	}
}

// An attribute name that cannot be a PAX record key (it holds "=") is left out and counted; the other attributes of the file are kept and
// the layer is written (tar.Writer refuses such a record, which used to fail every snapshot of the device).
func TestLayerUserXattrsSkipsNamesThatCannotBeExported(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "work", "", "content")
	setUserXattr(t, filepath.Join(f.merged, "work"), "user.good", "value")
	setUserXattr(t, filepath.Join(f.merged, "work"), "user.bad=name", "x")
	setUserXattr(t, filepath.Join(f.merged, "work"), "user.also=bad=more", "y")
	out, rep, err := f.enrichedReport(t, f.raw(t), snapshot.NewPolicy(0, nil, 0, 0))
	if err != nil {
		t.Fatalf("a name with = must not fail the layer: %v", err)
	}
	h := tarEntries(t, out)["work"]
	if h == nil {
		t.Fatal("the file is missing")
	}
	if h.PAXRecords[xattrPAXPrefix+"user.good"] != "value" {
		t.Fatalf("the exportable attribute was lost: %v", h.PAXRecords)
	}
	for k := range h.PAXRecords {
		if k == xattrPAXPrefix+"user.bad=name" || k == xattrPAXPrefix+"user.also=bad=more" {
			t.Fatalf("a name with = was exported: %q", k)
		}
	}
	if rep.BadNames != 2 {
		t.Fatalf("report: %+v", rep)
	}
}

func TestExportableXattrName(t *testing.T) {
	for name, want := range map[string]bool{"user.a": true, "user.": true, "user.a.b": true, "": false, "user.a=b": false, "user.a\x00b": false, "=": false} {
		if exportableXattrName(name) != want {
			t.Errorf("%q: want %v", name, want)
		}
	}
}
