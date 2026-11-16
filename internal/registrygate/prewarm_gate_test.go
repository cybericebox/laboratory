package registrygate

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"

	grpcserver "github.com/cybericebox/laboratory/internal/agent/grpc"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// putImage stores a real image (config, layer, manifest) in a repository of the fake upstream and returns the digest of its manifest.
func (r *rig) putImage(repo string) (string, v1.Image) {
	r.t.Helper()
	img, err := random.Image(2048, 1)
	if err != nil {
		r.t.Fatal(err)
	}
	cfg, _ := img.RawConfigFile()
	r.up.put(repo, "blobs", cfg)
	layers, _ := img.Layers()
	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			r.t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		r.up.put(repo, "blobs", body)
	}
	raw, _ := img.RawManifest()
	d := r.up.put(repo, "manifests", raw)
	r.up.mu.Lock()
	r.up.objects["/v2/"+repo+"/manifests/latest"] = raw
	r.up.mu.Unlock()
	return d, img
}

// prewarmer is the management agent's PrewarmImages, pointed at the real gate and zot of the rig: the registry "reg.test" is the fake
// upstream, and "alice" with "pw" may read org/private.
func (r *rig) prewarmer(t *testing.T) *grpcserver.Handler {
	t.Helper()
	r.cache.Registries.Set([]Registry{r.up.registry("up"), r.up.registry("reg.test")})
	h := grpcserver.NewHandler(nil, nil, nil)
	h.SetPrewarm(grpcserver.PrewarmConfig{
		Enabled: true, RegistryAddr: strings.TrimPrefix(r.gate.URL, "http://"), Registries: []string{"reg.test"},
		Timeout: 30 * time.Second, RetryAfter: time.Hour,
	})
	return h
}

func waitPrewarm(t *testing.T, h *grpcserver.Handler, img string, creds []*protobuf.RegistryCredential, want protobuf.PrewarmState) *protobuf.PrewarmImageStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		res, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{img}, RegistryCredentials: creds})
		if err != nil {
			t.Fatal(err)
		}
		if st := res.Images[0]; st.State == want {
			return st
		} else if time.Now().After(deadline) || st.State == protobuf.PrewarmState_PREWARM_STATE_FAILED {
			t.Fatalf("%s: %v (%s), want %v", img, st.State, st.Error, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The prewarm of an image that needs credentials goes through the gate with the credentials of the call, written the way the operator
// writes a pod's ("<tenant>~<user>"): the gate proves access upstream, fetches, stores; the call without them is refused by the gate.
func TestPrewarmWarmsAPrivateImageWithTheCallsCredentials(t *testing.T) {
	r := newRig(t)
	d, _ := r.putImage("org/private")
	h := r.prewarmer(t)
	creds := []*protobuf.RegistryCredential{{Registry: "reg.test", Username: "alice", Password: "pw"}}

	st := waitPrewarm(t, h, "reg.test/org/private:latest", creds, protobuf.PrewarmState_PREWARM_STATE_DONE)
	if st.Digest != d {
		t.Fatalf("digest %s, want %s", st.Digest, d)
	}
	if !r.inZot("c/reg.test/org/private", "manifests", d) {
		t.Fatal("the manifest is stored")
	}
	mfDigest, _ := r.cache.Store.Known(DecisionKey{Hash: r.cache.hash("reg.test", Creds{"alice", "pw"}), Repo: "reg.test/org/private"})
	if mfDigest.Ref == "" {
		t.Fatal("the gate proved the pull with the call's credentials, as for a pod")
	}

	// the same call without credentials is refused by the gate, and does not see the other's result
	res, err := h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"reg.test/org/private:latest"}})
	if err != nil {
		t.Fatal(err)
	}
	waitFailed := func() *protobuf.PrewarmImageStatus {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			res, _ = h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"reg.test/org/private:latest"}})
			if res.Images[0].State == protobuf.PrewarmState_PREWARM_STATE_FAILED {
				return res.Images[0]
			}
			if res.Images[0].State == protobuf.PrewarmState_PREWARM_STATE_DONE {
				t.Fatal("a call without credentials must not be told DONE for a private image")
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("no refusal")
		return nil
	}
	waitFailed()
	// other credentials: refused too (the gate asks upstream with them)
	bad := []*protobuf.RegistryCredential{{Registry: "reg.test", Username: "mallory", Password: "pw"}}
	res, _ = h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"reg.test/org/private:latest"}, RegistryCredentials: bad})
	for i := 0; i < 500 && res.Images[0].State != protobuf.PrewarmState_PREWARM_STATE_FAILED; i++ {
		time.Sleep(20 * time.Millisecond)
		res, _ = h.PrewarmImages(context.Background(), &protobuf.PrewarmImagesRequest{Images: []string{"reg.test/org/private:latest"}, RegistryCredentials: bad})
	}
	if res.Images[0].State != protobuf.PrewarmState_PREWARM_STATE_FAILED {
		t.Fatalf("a credential with no right upstream: %v", res.Images[0].State)
	}
}

// An image that is public is warmed without credentials, by the digest the resolver gives, and the next pull of a pod finds it in the cache.
func TestPrewarmWarmsAPublicImageThroughTheGate(t *testing.T) {
	r := newRig(t)
	d, img := r.putImage("library/nginx")
	r.up.mu.Lock()
	r.up.public["library/nginx"] = true
	r.up.mu.Unlock()
	h := grpcserver.NewHandler(nil, nil, nil)
	r.cache.Registries.Set([]Registry{r.up.registry("up"), r.up.registry("reg.test")})
	h.SetPrewarm(grpcserver.PrewarmConfig{
		Enabled: true, RegistryAddr: strings.TrimPrefix(r.gate.URL, "http://"), Registries: []string{"reg.test"},
		Resolver: staticResolver(d), Timeout: 30 * time.Second,
	})
	st := waitPrewarm(t, h, "reg.test/library/nginx:latest", nil, protobuf.PrewarmState_PREWARM_STATE_DONE)
	if st.Digest != d {
		t.Fatalf("%+v", st)
	}
	layers, _ := img.Layers()
	ld, _ := layers[0].Digest()
	if !r.inZot("c/reg.test/library/nginx", "blobs", ld.String()) {
		t.Fatal("the layer is stored")
	}
}

type staticResolver string

func (s staticResolver) Resolve(context.Context, string) (string, error) { return string(s), nil }
