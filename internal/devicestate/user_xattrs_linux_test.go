package devicestate

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/cybericebox/laboratory/internal/snapshot"
	"golang.org/x/sys/unix"
)

type userXattrFixture struct {
	lower, merged, upper string
	lo, live, up         *os.Root
}

func newUserXattrFixture(t *testing.T) *userXattrFixture {
	t.Helper()
	f := &userXattrFixture{lower: t.TempDir(), merged: t.TempDir(), upper: t.TempDir()}
	for _, item := range []struct {
		path string
		root **os.Root
	}{{f.lower, &f.lo}, {f.merged, &f.live}, {f.upper, &f.up}} {
		r, err := os.OpenRoot(item.path)
		if err != nil {
			t.Fatal(err)
		}
		*item.root = r
		t.Cleanup(func() { _ = r.Close() })
	}
	return f
}

func (f *userXattrFixture) file(t *testing.T, name, old, current string) {
	t.Helper()
	for _, item := range []struct{ root, body string }{{f.lower, old}, {f.merged, current}, {f.upper, current}} {
		p := filepath.Join(item.root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(item.body), 0644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(1700000000, 1)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
}

func setUserXattr(t *testing.T, p, name, value string) {
	t.Helper()
	if err := unix.Setxattr(p, name, []byte(value), 0); err != nil {
		t.Fatal(err)
	}
}

func getUserXattr(t *testing.T, p, name string) (string, error) {
	t.Helper()
	b := make([]byte, snapshot.MaxEntryHeaderBytes)
	n, err := unix.Getxattr(p, name, b)
	if err != nil {
		return "", err
	}
	return string(b[:n]), err
}

func TestLayerUserXattrsMetadataQuota(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "work", "", "")
	setUserXattr(t, filepath.Join(f.merged, "work"), "user.note", "metadata exceeds ten bytes")
	_, err := f.enriched(t, f.raw(t), snapshot.NewPolicy(0, nil, 10, 0))
	if !errors.Is(err, snapshot.ErrQuota) || !strings.Contains(err.Error(), "user xattr metadata") {
		t.Fatalf("metadata bypassed separate quota: %v", err)
	}
}

func (f *userXattrFixture) raw(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	if err := archive.WriteDiff(context.Background(), &raw, f.lower, f.merged); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func (f *userXattrFixture) enriched(t *testing.T, raw []byte, pol snapshot.Policy) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := writeUserXattrLayer(context.Background(), bytes.NewReader(raw), &out, f.up, f.live, f.lo, pol)
	return out.Bytes(), err
}

func tarHasPath(t *testing.T, b []byte, name string) bool {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == name {
			return true
		}
	}
}

func TestLayerUserXattrsEmittedFileRoundTrip(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "work", "old", "new content")
	setUserXattr(t, filepath.Join(f.merged, "work"), "user.note", "binary\x00value")
	if err := os.Link(filepath.Join(f.merged, "work"), filepath.Join(f.merged, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(f.upper, "work"), filepath.Join(f.upper, "hardlink")); err != nil {
		t.Fatal(err)
	}
	out, err := f.enriched(t, f.raw(t), snapshot.NewPolicy(0, nil, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Apply(context.Background(), f.lower, bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"work", "hardlink"} {
		got, err := getUserXattr(t, filepath.Join(f.lower, name), "user.note")
		if err != nil || got != "binary\x00value" {
			t.Fatalf("restored %s lost xattr: %q %v", name, got, err)
		}
	}
	a, _ := os.Stat(filepath.Join(f.lower, "work"))
	b, _ := os.Stat(filepath.Join(f.lower, "hardlink"))
	if !os.SameFile(a, b) {
		t.Fatal("xattr enrichment broke hard links")
	}
}

func TestLayerUserXattrsAttributeOnlyFileRoundTrip(t *testing.T) {
	for _, change := range []string{"add", "change", "remove"} {
		t.Run(change, func(t *testing.T) {
			f := newUserXattrFixture(t)
			f.file(t, "work", "same", "same")
			if change != "add" {
				setUserXattr(t, filepath.Join(f.lower, "work"), "user.note", "old")
			}
			if change != "remove" {
				setUserXattr(t, filepath.Join(f.merged, "work"), "user.note", "new")
			}
			raw := f.raw(t)
			if tarHasPath(t, raw, "work") {
				t.Fatal("fixture did not reproduce containerd's attribute-only omission")
			}
			out, err := f.enriched(t, raw, snapshot.NewPolicy(0, nil, 0, 0))
			if err != nil {
				t.Fatal(err)
			}
			if !tarHasPath(t, out, "work") {
				t.Fatal("attribute-only change omitted")
			}
			if _, err := archive.Apply(context.Background(), f.lower, bytes.NewReader(out)); err != nil {
				t.Fatal(err)
			}
			got, err := getUserXattr(t, filepath.Join(f.lower, "work"), "user.note")
			if change == "remove" {
				if !errors.Is(err, unix.ENODATA) {
					t.Fatalf("removed xattr survived: %q %v", got, err)
				}
			} else if err != nil || got != "new" {
				t.Fatalf("attribute-only change lost: %q %v", got, err)
			}
		})
	}
}

func TestLayerUserXattrsAttributeOnlyHardlinkRoundTrip(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "work", "same", "same")
	for _, root := range []string{f.lower, f.merged, f.upper} {
		if err := os.Link(filepath.Join(root, "work"), filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	setUserXattr(t, filepath.Join(f.merged, "work"), "user.note", "new")
	out, err := f.enriched(t, f.raw(t), snapshot.NewPolicy(0, nil, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Apply(context.Background(), f.lower, bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(filepath.Join(f.lower, "work"))
	b, _ := os.Stat(filepath.Join(f.lower, "link"))
	if !os.SameFile(a, b) {
		t.Fatal("attribute-only supplementation split the inode")
	}
	got, err := getUserXattr(t, filepath.Join(f.lower, "work"), "user.note")
	if err != nil || got != "new" {
		t.Fatalf("hardlink xattr lost: %q %v", got, err)
	}
}

func TestLayerUserXattrsDirectoryChangeAndRemoval(t *testing.T) {
	for _, change := range []string{"add", "change", "remove"} {
		t.Run(change, func(t *testing.T) {
			f := newUserXattrFixture(t)
			for _, root := range []string{f.lower, f.merged, f.upper} {
				if err := os.Mkdir(filepath.Join(root, "dir"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if change != "add" {
				setUserXattr(t, filepath.Join(f.lower, "dir"), "user.note", "old")
			}
			if change != "remove" {
				setUserXattr(t, filepath.Join(f.merged, "dir"), "user.note", "new")
			}
			out, err := f.enriched(t, f.raw(t), snapshot.NewPolicy(0, nil, 0, 0))
			if change == "remove" {
				if err == nil || !strings.Contains(err.Error(), "directory user xattr removal") {
					t.Fatalf("unsupported directory removal acknowledged: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := archive.Apply(context.Background(), f.lower, bytes.NewReader(out)); err != nil {
				t.Fatal(err)
			}
			got, err := getUserXattr(t, filepath.Join(f.lower, "dir"), "user.note")
			if err != nil || got != "new" {
				t.Fatalf("directory xattr lost: %q %v", got, err)
			}
		})
	}
}

func TestLayerUserXattrsBudgetsExclusionsAndRootEscape(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "work", "same", "same")
	setUserXattr(t, filepath.Join(f.merged, "work"), "user.note", "new")
	if _, err := f.enriched(t, f.raw(t), snapshot.NewPolicy(0, nil, 1, 0)); !errors.Is(err, snapshot.ErrQuota) {
		t.Fatalf("supplement bypassed quota: %v", err)
	}
	f.file(t, "other", "same", "same")
	setUserXattr(t, filepath.Join(f.merged, "other"), "user.note", "new")
	if _, err := f.enriched(t, f.raw(t), snapshot.NewPolicy(0, nil, 0, 0).WithMaxEntries(1)); !errors.Is(err, snapshot.ErrEntries) {
		t.Fatalf("supplement bypassed entry budget: %v", err)
	}
	huge := captureHeaderTar(t, &tar.Header{Name: "work", Typeflag: tar.TypeReg, Size: 4, PAXRecords: map[string]string{"large": strings.Repeat("x", snapshot.MaxEntryHeaderBytes+1)}})
	hugeOut, err := f.enriched(t, huge, snapshot.NewPolicy(0, nil, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var kept bytes.Buffer
	st, err := snapshot.FilterLayer(bytes.NewReader(hugeOut), &kept, snapshot.NewPolicy(0, nil, 0, 0))
	if err != nil || st.RefusedEntries != 1 || st.Dropped != 1 || st.Entries != 1 || st.Bytes != 4 || !tarHasPath(t, kept.Bytes(), "other") || tarHasPath(t, kept.Bytes(), "work") {
		t.Fatalf("raw header refusal changed: %+v %v", st, err)
	}
	pol := snapshot.NewPolicy(0, []string{"work", "other"}, 0, 0)
	out, err := f.enriched(t, f.raw(t), pol)
	if err != nil || tarHasPath(t, out, "work") || tarHasPath(t, out, "other") {
		t.Fatalf("excluded attributes read or emitted: %v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.merged, "escape")); err != nil {
		t.Fatal(err)
	}
	raw := captureHeaderTar(t, &tar.Header{Name: "escape/secret", Typeflag: tar.TypeReg, Size: 4})
	if _, err := f.enriched(t, raw, pol); err == nil {
		t.Fatal("root escape accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = writeUserXattrLayer(ctx, bytes.NewReader(f.raw(t)), io.Discard, f.up, f.live, f.lo, pol)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestLayerUserXattrsPreservesFilterSkipOrder(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "work", "old", "0123456789")
	setUserXattr(t, filepath.Join(f.merged, "work"), "user.note", "beyond tiny metadata quota")
	for _, root := range []string{f.merged, f.upper} {
		if err := os.Link(filepath.Join(root, "work"), filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	pol := snapshot.NewPolicy(0, nil, 1, 0).WithMaxFileSize(2).WithMaxEntries(1)
	out, err := f.enriched(t, f.raw(t), pol)
	if err != nil {
		t.Fatalf("existing size skip became a runtime failure: %v", err)
	}
	st, err := snapshot.FilterLayer(bytes.NewReader(out), io.Discard, pol)
	if err != nil || st.SkippedTotal != 1 || st.Entries != 0 || st.Bytes != 0 || st.Dropped != 2 {
		t.Fatalf("size/link skip or quota order changed: %+v %v", st, err)
	}
}

func TestManagedDiffReaderCleanup(t *testing.T) {
	for _, scenario := range []string{"eof", "close", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "held")
			if err := os.WriteFile(marker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if scenario == "timeout" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			}
			defer cancel()
			released := make(chan struct{})
			r := managedDiffReader(ctx, cancel, func(w io.Writer) error {
				body := "ok"
				if scenario != "eof" {
					body = strings.Repeat("x", 1<<16)
				}
				_, err := io.WriteString(w, body)
				return err
			}, func() {
				_ = os.Remove(marker)
				close(released)
			})
			if scenario == "eof" {
				got, err := io.ReadAll(r)
				if err != nil || string(got) != "ok" {
					t.Fatalf("stream completion: %q %v", got, err)
				}
			} else if scenario == "close" {
				if _, err := r.Read(make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
				if err := r.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			select {
			case <-released:
			case <-time.After(time.Second):
				t.Fatal("blocked producer retained resources")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("stream finished before cleanup")
			}
			if err := r.Close(); err != nil && (scenario == "eof" || !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatal(err)
			}
		})
	}
}

func TestLayerUserXattrsUSTARWithoutUserNamespace(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "work", "same", "same")
	setUserXattr(t, filepath.Join(f.merged, "work"), "user.note", "new")
	raw := captureHeaderTar(t, &tar.Header{Name: "work", Typeflag: tar.TypeReg, Size: 4, Mode: 0644, Uid: os.Getuid(), Gid: os.Getgid(), Format: tar.FormatUSTAR})
	in, err := tar.NewReader(bytes.NewReader(raw)).Next()
	if err != nil || in.Format != tar.FormatUSTAR {
		t.Fatalf("USTAR fixture missing: %+v %v", in, err)
	}
	out, err := f.enriched(t, raw, snapshot.NewPolicy(0, nil, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	h, err := tar.NewReader(bytes.NewReader(out)).Next()
	if err != nil || h.Format != tar.FormatPAX || h.PAXRecords["SCHILY.xattr.user.note"] != "new" || h.Uid != os.Getuid() || h.Gid != os.Getgid() {
		t.Fatalf("USTAR metadata upgrade changed owners or lost attrs: %+v %v", h, err)
	}
}

func TestLayerUserXattrsRootSwapRace(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "work", "same", "same")
	inside := filepath.Join(f.merged, "work")
	held := filepath.Join(f.merged, "held")
	outside := filepath.Join(t.TempDir(), "work")
	if err := os.WriteFile(outside, []byte("same"), 0644); err != nil {
		t.Fatal(err)
	}
	setUserXattr(t, inside, "user.note", "inside")
	setUserXattr(t, outside, "user.note", "outside")
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(inside, held); err != nil {
				return
			}
			_ = os.Symlink(outside, inside)
			_ = os.Remove(inside)
			_ = os.Rename(held, inside)
		}
	}()
	defer func() { close(stop); <-done }()
	raw := captureHeaderTar(t, &tar.Header{Name: "work", Typeflag: tar.TypeReg, Size: 4, Mode: 0644, Uid: os.Getuid(), Gid: os.Getgid()})
	for i := 0; i < 30; i++ {
		out, err := f.enriched(t, raw, snapshot.NewPolicy(0, nil, 0, 0))
		if err != nil {
			continue // concurrent inode mutation must fail closed
		}
		h, err := tar.NewReader(bytes.NewReader(out)).Next()
		if err != nil || h.PAXRecords["SCHILY.xattr.user.note"] != "inside" {
			t.Fatalf("root swap leaked outside inode metadata: %+v %v", h, err)
		}
	}
}

func TestDiffViewKeysSurviveFreshRuntime(t *testing.T) {
	first, restarted := &ContainerdRuntime{}, &ContainerdRuntime{}
	if first.diffViewKey("fixture") == restarted.diffViewKey("fixture") {
		t.Fatal("fresh runtime collided with a retained crash view")
	}
}

func TestLayerUserXattrsRequiredRefusalKeepsLatestImage(t *testing.T) {
	f := newUserXattrFixture(t)
	f.file(t, "work", "same", "same")
	f.file(t, "other", "same", "same")
	setUserXattr(t, filepath.Join(f.merged, "other"), "user.note", "new")
	huge := captureHeaderTar(t, &tar.Header{Name: "work", Typeflag: tar.TypeReg, Size: 4, PAXRecords: map[string]string{"large": strings.Repeat("x", snapshot.MaxEntryHeaderBytes+1)}})
	enriched, err := f.enriched(t, huge, snapshot.NewPolicy(0, nil, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(map[string]string{"saved": "kept"}))
	tr := r.track(t)
	defer r.e.stopAll()
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	before, _, _ := r.cl.snapshot()
	if len(before) != 1 {
		t.Fatal("latest image fixture missing")
	}
	r.rt.setDiff(enriched)
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err == nil || result.Result != "Failed" || result.Image != "" || result.Quiesced {
		t.Fatalf("refused enriched data acknowledged: %+v %v", result, err)
	}
	after, _, exits := r.cl.snapshot()
	if len(after) != 1 || after[0].Image != before[0].Image || tr.lastSnapshot.Image != before[0].Image || len(exits) != 0 {
		t.Fatal("required refusal replaced latest image or acknowledged exit")
	}
}

type footerFailureRuntime struct {
	*fakeRuntime
	failure error
}

func (r *footerFailureRuntime) Diff(ctx context.Context, _ Container, _ bool, _ snapshot.Policy) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	return managedDiffReader(ctx, cancel, func(out io.Writer) error {
		if _, err := out.Write(r.diff); err != nil {
			return err
		}
		return r.failure // a mounted producer can fail after tar end markers
	}, func() {}), nil
}

func TestLayerUserXattrsProducerCompletionFailure(t *testing.T) {
	for _, quota := range []int64{1, 1 << 20} {
		t.Run(fmt.Sprint(quota), func(t *testing.T) {
			r := newRig(t, time.Hour, quota)
			req := captureRequest(r)
			failure := errors.New("producer failed after tar footer")
			r.rt.setDiff(tarOf(map[string]string{"work": "data"}))
			r.e.Runtime = &footerFailureRuntime{fakeRuntime: r.rt, failure: failure}
			result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
			want := failure
			if quota == 1 {
				want = snapshot.ErrQuota // retain the original early filter failure
			}
			if !errors.Is(err, want) || result.Result != "Failed" || result.Image != "" || result.Quiesced {
				t.Fatalf("producer completion failure acknowledged: %+v %v", result, err)
			}
			images, _, exits := r.cl.snapshot()
			if len(images) != 0 || len(exits) != 0 {
				t.Fatal("producer failure published or acknowledged an exit")
			}
		})
	}
}
