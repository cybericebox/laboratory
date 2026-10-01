package grpc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/internal/snapshot"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

type tent struct {
	name string
	body string
	dir  bool
}

func writeTar(t *testing.T, dir, file string, ents ...tent) (string, int64) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var n int64
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(e.body))}
		if e.dir {
			h.Typeflag, h.Size, h.Mode = tar.TypeDir, 0, 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = tw.Write([]byte(e.body))
			n += h.Size
		}
	}
	_ = tw.Close()
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, n
}

// snapshotChain builds a snapshot image the way the node-agent does: a base image, then one
// snapshot layer per run, the later run starting from the earlier snapshot.
func snapshotChain(t *testing.T, runs ...[]tent) v1.Image {
	t.Helper()
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	pol := snapshot.NewPolicy(0, nil, 0, 50)
	for i, run := range runs {
		dir := t.TempDir()
		p, n := writeTar(t, dir, "layer.tar", run...)
		img, _, err = snapshot.Build(img, p, n, pol, dir)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	return img
}

type exported struct {
	meta    *protobuf.SnapshotMeta
	trailer *protobuf.SnapshotTrailer
	data    [][]byte
	order   []string
}

func (e *exported) send(c *protobuf.SnapshotChunk) error {
	switch v := c.Content.(type) {
	case *protobuf.SnapshotChunk_Meta:
		e.meta = v.Meta
		e.order = append(e.order, "meta")
	case *protobuf.SnapshotChunk_Data:
		e.data = append(e.data, v.Data)
		e.order = append(e.order, "data")
	case *protobuf.SnapshotChunk_Trailer:
		e.trailer = v.Trailer
		e.order = append(e.order, "trailer")
	}
	return nil
}

func (e *exported) archive(t *testing.T) (files map[string]string, order []string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(bytes.Join(e.data, nil)))
	if err != nil {
		t.Fatal(err)
	}
	files = map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
		order = append(order, h.Name)
	}
}

func TestExportSquashesTheChainKeepingWhiteouts(t *testing.T) {
	img := snapshotChain(t,
		[]tent{{name: "etc/app.conf", body: "v1"}, {name: "data/old", body: "old"}, {name: "data/keep", body: "keep"}},
		[]tent{{name: "etc/app.conf", body: "v2"}, {name: "data/.wh.old"}, {name: "srv/new", body: "new"}},
	)
	e := &exported{}
	meta := &protobuf.SnapshotMeta{Ref: &protobuf.ItemRef{LabGroup: "g", Lab: "l", Name: "web"}, BaseImage: "nginx:1"}
	if err := exportImage(context.Background(), img, meta, 1<<20, e.send); err != nil {
		t.Fatal(err)
	}
	files, order := e.archive(t)
	if files["etc/app.conf"] != "v2" || files["srv/new"] != "new" || files["data/keep"] != "keep" {
		t.Fatalf("the newest version of every file, nothing of the layer history: %v", files)
	}
	if _, ok := files["data/old"]; ok {
		t.Fatal("a file deleted in a later run must not be in the archive")
	}
	if _, ok := files["data/.wh.old"]; !ok {
		t.Fatalf("the deletion must stay as an OCI whiteout so it also hides the base image's file: %v", files)
	}
	if order[0] != "data/.wh.old" {
		t.Fatalf("whiteouts come first: %v", order)
	}
	if e.meta.SquashedLayers != 2 || len(e.meta.BaseLayerDigests) != 2 || e.meta.SnapshotDigest == "" || e.meta.Format == "" || e.meta.BaseImage != "nginx:1" {
		t.Fatalf("meta %+v", e.meta)
	}
}

func TestExportMessagesAreMetaThenChunksThenTrailer(t *testing.T) {
	big := strings.Repeat("0123456789abcdef", 4000) // incompressible enough with random below
	img := snapshotChain(t, []tent{{name: "blob", body: randomText(60000)}, {name: "z", body: big}})
	e := &exported{}
	if err := exportImage(context.Background(), img, &protobuf.SnapshotMeta{}, 4096, e.send); err != nil {
		t.Fatal(err)
	}
	if e.order[0] != "meta" || e.order[len(e.order)-1] != "trailer" {
		t.Fatalf("order %v", e.order)
	}
	if len(e.data) < 3 {
		t.Fatalf("the archive should span several chunks, got %d", len(e.data))
	}
	var total int64
	for i, d := range e.data {
		total += int64(len(d))
		if i < len(e.data)-1 && len(d) != 4096 {
			t.Fatalf("chunk %d has %d bytes, want 4096", i, len(d))
		}
	}
	sum := sha256.Sum256(bytes.Join(e.data, nil))
	if e.trailer.CompressedBytes != total || e.trailer.Sha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("trailer %+v does not match the stream (%d bytes)", e.trailer, total)
	}
	files, _ := e.archive(t)
	if len(files["blob"]) != 60000 {
		t.Fatalf("content lost: %d", len(files["blob"]))
	}
	if e.meta.ChunkSize != 4096 {
		t.Fatalf("meta chunk size %d", e.meta.ChunkSize)
	}
}

func randomText(n int) string {
	b := make([]byte, n)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte('a' + x%26)
	}
	return string(b)
}

func TestExportOfABaseImageIsRefused(t *testing.T) {
	base, _ := random.Image(256, 2)
	err := exportImage(context.Background(), base, &protobuf.SnapshotMeta{}, 1024, (&exported{}).send)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an image without snapshot layers has nothing to export: %v", err)
	}
}

func TestExportStopsWhenTheCallerGoesAway(t *testing.T) {
	img := snapshotChain(t, []tent{{name: "blob", body: randomText(200000)}})
	ctx, cancel := context.WithCancel(context.Background())
	sent := 0
	err := exportImage(ctx, img, &protobuf.SnapshotMeta{}, 512, func(c *protobuf.SnapshotChunk) error {
		if _, ok := c.Content.(*protobuf.SnapshotChunk_Data); ok {
			sent++
			if sent == 2 {
				cancel()
			}
		}
		return nil
	})
	if err == nil || sent > 3 {
		t.Fatalf("the export must stop soon after the context ends: err=%v sent=%d", err, sent)
	}
}

// ---- through the handler: a Device CR and a registry -------------------------------------

type exportStream struct {
	grpc.ServerStream
	ctx context.Context
	e   *exported
}

func (s *exportStream) Context() context.Context             { return s.ctx }
func (s *exportStream) Send(c *protobuf.SnapshotChunk) error { return s.e.send(c) }

type exportRig struct {
	h    *Handler
	reg  string
	repo string
}

func newExportRig(t *testing.T, persistence bool, snapshotImage v1.Image) *exportRig {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	cs := fake.NewSimpleClientset()
	h := NewHandler(cs, k8sfake.NewSimpleClientset(), nil)
	h.SetRegistryAddr(host)
	ctx := context.Background()
	if _, err := cs.LaboratoryV1alpha1().LabGroups().Create(ctx, &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: crName("grp")},
		Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: "ns-grp"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dev := &laboratoryv1alpha1.Device{
		ObjectMeta: metav1.ObjectMeta{Name: crName("lab1") + "-web", Namespace: "ns-grp"},
		Spec:       laboratoryv1alpha1.DeviceSpec{LabRef: crName("lab1"), Name: "web", Image: "nginx:1", ImageDigests: map[string]string{"nginx:1": "sha256:" + strings.Repeat("b", 64)}},
	}
	if persistence {
		dev.Spec.State = &laboratoryv1alpha1.DeviceStateSpec{Enabled: true}
	}
	rig := &exportRig{h: h, reg: host, repo: "lab/ns-grp/" + crName("lab1") + "/web"}
	if snapshotImage != nil {
		ref, err := name.ParseReference(host+"/"+rig.repo+":latest", name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, snapshotImage); err != nil {
			t.Fatal(err)
		}
		d, _ := snapshotImage.Digest()
		at := metav1.NewTime(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
		dev.Status.State = &laboratoryv1alpha1.DeviceStateStatus{Image: "localhost:5035/" + rig.repo + "@" + d.String(), SnapshotAt: &at}
	}
	if _, err := cs.LaboratoryV1alpha1().Devices("ns-grp").Create(ctx, dev, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r *exportRig) export(ref *protobuf.ItemRef) (*exported, error) {
	e := &exported{}
	err := r.h.ExportDeviceSnapshot(&protobuf.DeviceSnapshotRequest{Ref: ref}, &exportStream{ctx: context.Background(), e: e})
	return e, err
}

var webRef = &protobuf.ItemRef{LabGroup: "grp", Lab: "lab1", Name: "web"}

func TestExportDeviceSnapshotFromTheRegistry(t *testing.T) {
	img := snapshotChain(t,
		[]tent{{name: "home/user/notes.txt", body: "first"}},
		[]tent{{name: "home/user/notes.txt", body: "second"}, {name: "home/.wh.gone"}},
	)
	rig := newExportRig(t, true, img)
	e, err := rig.export(webRef)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := e.archive(t)
	if files["home/user/notes.txt"] != "second" || files["home/.wh.gone"] != "" {
		t.Fatalf("files %v", files)
	}
	if _, ok := files["home/.wh.gone"]; !ok {
		t.Fatal("the whiteout must be exported")
	}
	m := e.meta
	if m.Ref.GetName() != "web" || m.BaseImage != "nginx:1" || m.BaseImageDigest != "sha256:"+strings.Repeat("b", 64) ||
		m.SnapshotUnixMs != time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).UnixMilli() || !strings.HasPrefix(m.SnapshotDigest, "sha256:") || m.SquashedLayers != 2 {
		t.Fatalf("meta %+v", m)
	}
}

func TestExportDeviceSnapshotRefusals(t *testing.T) {
	img := snapshotChain(t, []tent{{name: "f", body: "x"}})

	off := newExportRig(t, false, img)
	if _, err := off.export(webRef); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "no state persistence") {
		t.Fatalf("persistence off: %v", err)
	}
	none := newExportRig(t, true, nil)
	if _, err := none.export(webRef); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "no snapshot yet") {
		t.Fatalf("no snapshot: %v", err)
	}
	noReg := newExportRig(t, true, img)
	noReg.h.SetRegistryAddr("")
	if _, err := noReg.export(webRef); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "registry is not configured") {
		t.Fatalf("no registry: %v", err)
	}
	ok := newExportRig(t, true, img)
	if _, err := ok.export(&protobuf.ItemRef{LabGroup: "grp", Lab: "lab1", Name: "nope"}); status.Code(err) != codes.NotFound && !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown device: %v", err)
	}
	if _, err := ok.export(&protobuf.ItemRef{LabGroup: "grp", Lab: "lab1"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("incomplete ref: %v", err)
	}
	// A snapshot that holds no changes (the status points at a plain image).
	base, _ := random.Image(128, 1)
	empty := newExportRig(t, true, base)
	if _, err := empty.export(webRef); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an image without snapshot layers: %v", err)
	}
}

func TestSplitSnapshotRef(t *testing.T) {
	repo, d, err := splitSnapshotRef("localhost:5035/lab/ns/lab1/web@sha256:abc")
	if err != nil || repo != "lab/ns/lab1/web" || d != "sha256:abc" {
		t.Fatalf("%q %q %v", repo, d, err)
	}
	if _, _, err := splitSnapshotRef("localhost:5035/lab/x:tag"); err == nil {
		t.Fatal("a tag reference is not a snapshot reference")
	}
}
