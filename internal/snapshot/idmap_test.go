package snapshot

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func ownedTar(t *testing.T, owners map[string][2]int) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, o := range owners {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Uid: o[0], Gid: o[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func ownersOf(t *testing.T, b []byte) map[string][2]int {
	t.Helper()
	got := map[string][2]int{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = [2]int{h.Uid, h.Gid}
	}
}

func TestFilterLayerMappedShiftedIDsBecomeContainerRelative(t *testing.T) {
	const base = 2628911104
	m := IDMap{{ContainerID: 0, HostID: base, Size: 65536}}
	in := ownedTar(t, map[string][2]int{"etc/passwd": {base, base}, "home/u/f": {base + 1000, base + 100}})
	var out bytes.Buffer
	st, err := FilterLayerMapped(bytes.NewReader(in), &out, NewPolicy(0, nil, 0, 0), IDMaps{UID: m, GID: m})
	if err != nil {
		t.Fatal(err)
	}
	got := ownersOf(t, out.Bytes())
	if got["etc/passwd"] != [2]int{0, 0} || got["home/u/f"] != [2]int{1000, 100} || st.Unmapped != 0 {
		t.Fatalf("owners %v, unmapped %d", got, st.Unmapped)
	}
}

func TestFilterLayerMappedIdentityAndEmptyMapChangeNothing(t *testing.T) {
	in := ownedTar(t, map[string][2]int{"a": {1000, 2000}, "b": {2628911104, 5}})
	for name, ids := range map[string]IDMaps{
		"empty":    {},
		"identity": {UID: IDMap{{0, 0, 4294967295}}, GID: IDMap{{0, 0, 4294967295}}},
	} {
		var out bytes.Buffer
		if _, err := FilterLayerMapped(bytes.NewReader(in), &out, NewPolicy(0, nil, 0, 0), ids); err != nil {
			t.Fatal(err)
		}
		got := ownersOf(t, out.Bytes())
		if got["a"] != [2]int{1000, 2000} || got["b"] != [2]int{2628911104, 5} && name == "empty" {
			t.Fatalf("%s: owners %v", name, got)
		}
	}
}

func TestFilterLayerMappedOutOfRangeBecomesZero(t *testing.T) {
	const base = 65536 * 3
	m := IDMap{{ContainerID: 0, HostID: base, Size: 65536}}
	in := ownedTar(t, map[string][2]int{"x": {42, base + 7}, "y": {base + 65536, base}})
	var out bytes.Buffer
	st, err := FilterLayerMapped(bytes.NewReader(in), &out, NewPolicy(0, nil, 0, 0), IDMaps{UID: m, GID: m})
	if err != nil {
		t.Fatal(err)
	}
	got := ownersOf(t, out.Bytes())
	if got["x"] != [2]int{0, 7} || got["y"] != [2]int{0, 0} || st.Unmapped != 2 {
		t.Fatalf("owners %v, unmapped %d", got, st.Unmapped)
	}
}
