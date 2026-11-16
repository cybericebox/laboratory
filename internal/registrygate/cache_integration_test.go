package registrygate

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	alice  = "t1~alice"
	aliceT = "t2~alice" // the same credentials as alice, another tenant
	carol  = "t3~carol"
	mal    = "t4~mallory"
)

func mpath(repo, digest string) string { return "/v2/c/up/" + repo + "/manifests/" + digest }
func bpath(repo, digest string) string { return "/v2/c/up/" + repo + "/blobs/" + digest }

// g1: an anonymous image is served without a 401 and filled once.
func TestAnonymousImageIsServedAndFilledOnce(t *testing.T) {
	r := newRig(t)
	layer := bytes.Repeat([]byte("L"), 4096)
	ld := r.up.put("library/nginx", "blobs", layer)
	cfg := r.up.put("library/nginx", "blobs", []byte("{}"))
	md := r.up.put("library/nginx", "manifests", manifestBody(cfg, ld))

	code, body, hdr := r.pull("GET", mpath("library/nginx", md), "", "", nil)
	if code != 200 || hdr.Get("Docker-Content-Digest") != md || !bytes.Contains(body, []byte(ld)) {
		t.Fatalf("manifest: %d %q", code, hdr)
	}
	if code, body, _ := r.pull("GET", bpath("library/nginx", ld), "", "", nil); code != 200 || !bytes.Equal(body, layer) {
		t.Fatalf("blob: %d", code)
	}
	if r.inZot("c/up/library/nginx", "manifests", md) {
		t.Fatal("zot refuses a manifest before its blobs: it waits in the gate until the last one is stored")
	}
	if code, _, _ := r.pull("GET", bpath("library/nginx", cfg), "", "", nil); code != 200 {
		t.Fatalf("config: %d", code)
	}
	getsBefore := r.up.n("GET library/nginx")
	for i := 0; i < 3; i++ {
		if code, body, _ := r.pull("GET", bpath("library/nginx", ld), "", "", nil); code != 200 || !bytes.Equal(body, layer) {
			t.Fatalf("blob again: %d", code)
		}
		if code, _, _ := r.pull("GET", mpath("library/nginx", md), "", "", nil); code != 200 {
			t.Fatalf("manifest again: %d", code)
		}
	}
	if r.up.n("GET library/nginx") != getsBefore {
		t.Fatalf("a cached object must not be fetched again: %d -> %d", getsBefore, r.up.n("GET library/nginx"))
	}
	if !r.inZot("c/up/library/nginx", "blobs", ld) || !r.inZot("c/up/library/nginx", "manifests", md) {
		t.Fatal("the object must be stored in zot under c/up/...")
	}
	if !r.inZot("c/up/library/nginx", "manifests", "sha256-"+strings.TrimPrefix(md, "sha256:")) {
		t.Fatal("a cached manifest carries a tag of its own (zot retention works on tags)")
	}
}

// g2 g3 g4: a private image: the 401 first, the credentials of the pod, one download; another credential is refused even for a digest that
// is in the cache; the same credentials of another tenant are checked once more.
func TestPrivateImageIsProvedPerPull(t *testing.T) {
	r := newRig(t)
	layer := bytes.Repeat([]byte("P"), 5000)
	ld := r.up.put("org/private", "blobs", layer)
	md := r.up.put("org/private", "manifests", manifestBody(r.up.put("org/private", "blobs", []byte("{}")), ld))

	if code, _, _ := r.pull("GET", mpath("org/private", md), "", "", nil); code != 401 {
		t.Fatalf("a private image without credentials: %d", code)
	}
	if code, _, _ := r.pull("GET", mpath("org/private", md), alice, "pw", nil); code != 200 {
		t.Fatalf("alice: %d", code)
	}
	if code, body, _ := r.pull("GET", bpath("org/private", ld), alice, "pw", nil); code != 200 || !bytes.Equal(body, layer) {
		t.Fatalf("alice's blob: %d", code)
	}
	if r.up.n("GET org/private") != 2 { // the manifest and the blob, once
		t.Fatalf("upstream GETs %d", r.up.n("GET org/private"))
	}

	// g3: mallory has an account upstream, no right to this repository: nothing of the cached copy reaches her
	for _, p := range []string{mpath("org/private", md), bpath("org/private", ld)} {
		if code, body, _ := r.pull("GET", p, mal, "pw", nil); code != 401 || bytes.Contains(body, layer) {
			t.Fatalf("mallory %s: %d", p, code)
		}
	}
	if code, _, _ := r.pull("GET", bpath("org/private", ld), "t9~alice", "wrong", nil); code != 401 {
		t.Fatalf("a wrong password: %d", code)
	}

	// g4: the same credentials, another tenant: a fresh check at once, then the decision is reused
	heads := r.up.n("HEAD org/private")
	if code, _, _ := r.pull("GET", bpath("org/private", ld), aliceT, "pw", nil); code != 200 {
		t.Fatalf("another tenant with the same credentials: %d", code)
	}
	if r.up.n("HEAD org/private") != heads+1 {
		t.Fatalf("a new tenant for the hash is a mandatory fresh check: HEADs %d -> %d", heads, r.up.n("HEAD org/private"))
	}
	if code, _, _ := r.pull("GET", bpath("org/private", ld), aliceT, "pw", nil); code != 200 || r.up.n("HEAD org/private") != heads+1 {
		t.Fatalf("inside max-age the decision is reused: %d", r.up.n("HEAD org/private"))
	}
}

// g5: a credential revoked upstream: a verified tenant keeps working until the next check, then every tenant of the hash is refused at once.
func TestRevokedCredentialsAreRefusedForEveryTenantOfTheHash(t *testing.T) {
	r := newRig(t)
	ld := r.up.put("org/private", "blobs", []byte("layer"))
	for _, tenant := range []string{alice, aliceT} {
		if code, _, _ := r.pull("GET", bpath("org/private", ld), tenant, "pw", nil); code != 200 {
			t.Fatalf("%s: %d", tenant, code)
		}
	}
	r.up.mu.Lock()
	r.up.repos["org/private"] = map[string]bool{} // alice loses the repository
	r.up.mu.Unlock()
	r.clock.advance(3 * time.Minute) // the decision is stale
	if code, _, _ := r.pull("GET", bpath("org/private", ld), alice, "pw", nil); code != 200 {
		t.Fatalf("a verified tenant is served stale while it is revalidated: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.cache.Store.Lookup(DecisionKey{Hash: r.cache.hash("up", Creds{"alice", "pw"}), Repo: "up/org/private"}, "t1") != Refuse {
		if time.Now().After(deadline) {
			t.Fatal("the background revalidation did not refuse the revoked credentials")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, tenant := range []string{alice, aliceT} {
		if code, _, _ := r.pull("GET", bpath("org/private", ld), tenant, "pw", nil); code != 401 {
			t.Fatalf("after the revalidation every tenant of the hash is refused at once, %s: %d", tenant, code)
		}
	}
}

// g6: upstream down.
func TestUpstreamOutage(t *testing.T) {
	r := newRig(t)
	pub := r.up.put("library/nginx", "blobs", []byte("public layer"))
	priv := r.up.put("org/private", "blobs", []byte("private layer"))
	r.pull("GET", bpath("library/nginx", pub), "", "", nil)
	r.pull("GET", bpath("org/private", priv), alice, "pw", nil)

	r.up.setFail(503)
	r.clock.advance(5 * time.Minute)
	if code, _, _ := r.pull("GET", bpath("org/private", priv), alice, "pw", nil); code != 200 {
		t.Fatalf("a verified tenant inside stale-if-error: %d", code)
	}
	if code, _, _ := r.pull("GET", bpath("library/nginx", pub), "", "", nil); code != 200 {
		t.Fatalf("an anonymous image allowed earlier: %d", code)
	}
	if code, body, _ := r.pull("GET", bpath("org/private", priv), carol, "pw", nil); code != 503 || !strings.Contains(string(body), reasonUnavailable) {
		t.Fatalf("a new tenant during the outage: %d %s", code, body)
	}
	r.clock.advance(10 * time.Minute)
	if code, _, _ := r.pull("GET", bpath("org/private", priv), alice, "pw", nil); code != 503 {
		t.Fatalf("a verified tenant older than stale-if-error: %d", code)
	}
	r.clock.advance(10 * time.Hour)
	if code, _, _ := r.pull("GET", bpath("library/nginx", pub), "", "", nil); code != 200 {
		t.Fatalf("a public image is served for the whole outage: %d", code)
	}
}

// g7: an empty-scope token is no proof.
func TestEmptyScopeTokenIsRefused(t *testing.T) {
	r := newRig(t)
	r.up.emptyScope = true
	ld := r.up.put("org/private", "blobs", []byte("secret"))
	if code, _, _ := r.pull("GET", bpath("org/private", ld), mal, "pw", nil); code != 401 {
		t.Fatalf("%d", code)
	}
}

// g8: twelve first pulls of one layer: one download, twelve answers.
func TestConcurrentFirstPullsDownloadOnce(t *testing.T) {
	r := newRig(t)
	layer := bytes.Repeat([]byte("C"), 1<<20)
	ld := r.up.put("org/private", "blobs", layer)
	var wg sync.WaitGroup
	errs := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body, _ := r.pull("GET", bpath("org/private", ld), alice, "pw", nil)
			if code != 200 || !bytes.Equal(body, layer) {
				errs <- fmt.Sprintf("%d %d", code, len(body))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("a pull failed: %s", e)
	}
	if n := r.up.n("GET org/private"); n != 1 {
		t.Fatalf("one upstream download per blob, got %d", n)
	}
	if !r.inZot("c/up/org/private", "blobs", ld) {
		t.Fatal("not stored")
	}
}

// g9: a layer shared by two repositories costs no second download.
func TestSharedLayerIsMountedNotDownloaded(t *testing.T) {
	r := newRig(t)
	r.up.repos["org/other"] = map[string]bool{"alice": true}
	shared := bytes.Repeat([]byte("S"), 2048)
	sd := r.up.put("org/private", "blobs", shared)
	r.up.put("org/other", "blobs", shared)
	cfg := r.up.put("org/private", "blobs", []byte("{}"))
	r.up.put("org/other", "blobs", []byte("{}"))
	ma := r.up.put("org/private", "manifests", manifestBody(cfg, sd))
	mb := r.up.put("org/other", "manifests", manifestBody(cfg, sd))

	for _, p := range []string{mpath("org/private", ma), bpath("org/private", sd)} {
		if code, _, _ := r.pull("GET", p, alice, "pw", nil); code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
	}
	for _, p := range []string{mpath("org/other", mb), bpath("org/other", sd)} {
		if code, _, _ := r.pull("GET", p, alice, "pw", nil); code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
	}
	if n := r.up.n("GET org/other"); n != 1 { // the manifest only: the layer was linked in
		t.Fatalf("the shared layer must not be downloaded again, upstream GETs of org/other: %d", n)
	}
	if !r.inZot("c/up/org/other", "blobs", sd) {
		t.Fatal("the layer must be in the second repository")
	}
	// a layer no manifest of the repository lists is NOT linked in: it is asked for upstream (and refused there)
	r.up.put("org/private", "blobs", []byte("only in private"))
	hidden := r.up.put("org/private", "blobs", []byte("a digest the other repository does not list"))
	if code, _, _ := r.pull("GET", bpath("org/private", hidden), alice, "pw", nil); code != 200 {
		t.Fatal(code)
	}
	if code, _, _ := r.pull("GET", bpath("org/other", hidden), alice, "pw", nil); code != 404 && code != 401 {
		t.Fatalf("a blob that org/other does not list must not appear there: %d", code)
	}
}

// g10: a Range on a miss is answered after the fill, from the stored copy; a CDN redirect carries no Authorization.
func TestRangeOnAMissAndCDNRedirect(t *testing.T) {
	r := newRig(t)
	r.up.redirect = true
	layer := []byte("0123456789abcdefghij")
	ld := r.up.put("org/private", "blobs", layer)
	code, body, hdr := r.pull("GET", bpath("org/private", ld), alice, "pw", map[string]string{"Range": "bytes=5-9"})
	if code != 206 || string(body) != "56789" {
		t.Fatalf("range: %d %q %v", code, body, hdr)
	}
	if !r.inZot("c/up/org/private", "blobs", ld) {
		t.Fatal("the blob must be stored before the range is answered")
	}
	for _, a := range r.up.authSeen {
		if a != "" {
			t.Fatalf("Authorization must not go to the CDN: %q", a)
		}
	}
	if r.up.n("cdn") != 1 {
		t.Fatalf("one CDN download: %d", r.up.n("cdn"))
	}
}

// g12: what the cache path does not serve.
func TestCachePathRefusals(t *testing.T) {
	r := newRig(t)
	ld := r.up.put("org/private", "blobs", []byte("x"))
	type tc struct {
		method, path string
		want         int
	}
	for _, c := range []tc{
		{"POST", "/v2/c/up/org/private/blobs/uploads/", 405},
		{"PUT", mpath("org/private", ld), 405},
		{"DELETE", mpath("org/private", ld), 405},
		{"GET", "/v2/c/up/org/private/tags/list", 404},
		{"GET", "/v2/c/up/_catalog", 404},
		{"GET", "/v2/c/up/org/private/referrers/" + ld, 404},
		{"GET", "/v2/c/nowhere/org/private/blobs/" + ld, 403},
		{"GET", "/v2/c/up/org/%2e%2e/private/blobs/" + ld, 400},
		{"GET", "/v2/c/up/org/private/blobs/" + ld + "/../x", 400},
		{"GET", "/v2/c//up/org/private/blobs/" + ld, 400},
		{"GET", "/v2/c/up/org%2Fprivate/blobs/" + ld, 400},
	} {
		req, _ := http.NewRequest(c.method, r.gate.URL+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
	// zot itself is not reachable through the pass-through for the cache tree: the cache path never goes there
	resp, err := http.Get(r.gate.URL + "/v2/_catalog")
	if err == nil {
		defer resp.Body.Close()
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(resp.Body)
		if strings.Contains(b.String(), "c/up") {
			t.Fatalf("the catalog must not list the cache tree to a client: %s", b)
		}
	}
}

// r2: the disk is above its high-water mark: a miss is streamed through, unstored; a hit is served.
func TestDiskFullStreamsThroughWithoutStoring(t *testing.T) {
	r := newRig(t)
	stored := r.up.put("org/private", "blobs", []byte("already cached"))
	fresh := r.up.put("org/private", "blobs", []byte("not yet cached"))
	r.pull("GET", bpath("org/private", stored), alice, "pw", nil)
	r.cache.DiskFull = func() bool { return true }
	if code, body, _ := r.pull("GET", bpath("org/private", fresh), alice, "pw", nil); code != 200 || string(body) != "not yet cached" {
		t.Fatalf("a miss streams through: %d %q", code, body)
	}
	if r.inZot("c/up/org/private", "blobs", fresh) {
		t.Fatal("nothing is stored while the disk is full")
	}
	getsBefore := r.up.n("GET org/private")
	if code, body, _ := r.pull("GET", bpath("org/private", stored), alice, "pw", nil); code != 200 || string(body) != "already cached" {
		t.Fatalf("a hit is served: %d", code)
	}
	if r.up.n("GET org/private") != getsBefore {
		t.Fatal("a hit must not go upstream")
	}
}

// A tag is resolved upstream with the pull's credentials every time; the answer carries the digest.
func TestTagIsResolvedUpstreamEveryTime(t *testing.T) {
	r := newRig(t)
	body := manifestBody(r.up.put("org/private", "blobs", []byte("{}")))
	d := r.up.put("org/private", "manifests", body)
	r.up.mu.Lock()
	r.up.objects["/v2/org/private/manifests/v1"] = body
	r.up.mu.Unlock()
	heads := r.up.n("HEAD org/private")
	for i := 0; i < 2; i++ {
		code, got, hdr := r.pull("GET", mpath("org/private", "v1"), alice, "pw", nil)
		if code != 200 || hdr.Get("Docker-Content-Digest") != d || !bytes.Equal(got, body) {
			t.Fatalf("tag pull %d: %d %v", i, code, hdr)
		}
	}
	if r.up.n("HEAD org/private")-heads < 2 {
		t.Fatal("a tag is never answered from the gate's memory")
	}
	if code, _, _ := r.pull("GET", mpath("org/private", "v1"), mal, "pw", nil); code != 401 {
		t.Fatalf("a tag of a private image for a credential without a right: %d", code)
	}
}

// An index of several platforms can never be complete in zot (one platform's children are pulled), so it is answered from the gate's
// memory after the first fetch; its image manifest is stored in zot once its blobs are.
func TestIndexIsServedFromMemoryAndChildIsStored(t *testing.T) {
	r := newRig(t)
	layer := []byte("platform layer")
	ld := r.up.put("org/private", "blobs", layer)
	cfg := r.up.put("org/private", "blobs", []byte("{}"))
	child := r.up.put("org/private", "manifests", manifestBody(cfg, ld))
	other := "sha256:" + strings.Repeat("e", 64) // another platform, never pulled
	idx := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":1},`+
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":1}]}`, child, other))
	id := r.up.put("org/private", "manifests", idx)

	accept := map[string]string{"Accept": "application/vnd.oci.image.index.v1+json"}
	for i := 0; i < 3; i++ {
		code, body, _ := r.pull("GET", mpath("org/private", id), alice, "pw", accept)
		if code != 200 || !bytes.Equal(body, idx) {
			t.Fatalf("index %d: %d", i, code)
		}
	}
	if n := r.up.n("GET org/private"); n != 1 {
		t.Fatalf("the index is fetched once and then answered from memory: %d GETs", n)
	}
	for _, p := range []string{mpath("org/private", child), bpath("org/private", cfg), bpath("org/private", ld)} {
		if code, _, _ := r.pull("GET", p, alice, "pw", nil); code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
	}
	if !r.inZot("c/up/org/private", "manifests", child) {
		t.Fatal("the image manifest is stored once its blobs are")
	}
	if r.inZot("c/up/org/private", "manifests", id) {
		t.Fatal("an index with children that were never pulled cannot be stored")
	}
	// still answered, for the credential that has the right, and only for it
	if code, _, _ := r.pull("GET", mpath("org/private", id), mal, "pw", accept); code != 401 {
		t.Fatalf("the memory copy is behind the proof like the stored ones: %d", code)
	}
}

// A digest that a manifest lists is no proof that the repository has the blob: whoever controls an upstream repository can list any
// digest in a manifest of his own. A blob that another cached repository holds is linked into this one only after a HEAD of that exact
// digest in THIS repository with the caller's own credentials.
func TestAListedDigestIsNoProofAndNothingIsMountedWithoutOne(t *testing.T) {
	r := newRig(t)
	r.up.users["eve"] = "pw"
	r.up.repos["org/evil"] = map[string]bool{"eve": true}
	r.up.repos["org/private"] = map[string]bool{"carol": true} // the victim's repository
	secret := bytes.Repeat([]byte("victim's layer"), 100)
	sd := r.up.put("org/private", "blobs", secret)
	// the victim's blob is in the cache (carol pulled it)
	if code, _, _ := r.pull("GET", bpath("org/private", sd), carol, "pw", nil); code != 200 {
		t.Fatalf("carol: %d", code)
	}
	if !r.inZot("c/up/org/private", "blobs", sd) {
		t.Fatal("the victim's layer is cached")
	}
	// eve's repository has a manifest that LISTS the victim's digest (a registry that does not check what a manifest refers to)
	cfg := r.up.put("org/evil", "blobs", []byte("{}"))
	mb := r.up.put("org/evil", "manifests", manifestBody(cfg, sd))
	if code, _, _ := r.pull("GET", mpath("org/evil", mb), "t5~eve", "pw", nil); code != 200 {
		t.Fatalf("eve's own manifest: %d", code)
	}
	headsBefore := r.up.n("HEAD org/evil")
	code, body, _ := r.pull("GET", bpath("org/evil", sd), "t5~eve", "pw", nil)
	if code == 200 || bytes.Contains(body, secret) {
		t.Fatalf("the victim's layer must not be served to eve through her own manifest: %d", code)
	}
	if r.inZot("c/up/org/evil", "blobs", sd) {
		t.Fatal("the victim's layer must not be linked into eve's repository")
	}
	if r.up.n("HEAD org/evil") == headsBefore {
		t.Fatal("the gate must ask upstream whether THIS repository has THIS blob")
	}
}

// The proof is one HEAD per credential, repository and digest, and then a legitimate shared layer is linked in without a download.
func TestMountNeedsAndCachesTheBlobProof(t *testing.T) {
	r := newRig(t)
	r.up.repos["org/other"] = map[string]bool{"alice": true}
	shared := bytes.Repeat([]byte("S"), 2048)
	sd := r.up.put("org/private", "blobs", shared)
	r.up.put("org/other", "blobs", shared)
	if code, _, _ := r.pull("GET", bpath("org/private", sd), alice, "pw", nil); code != 200 {
		t.Fatal(code)
	}
	gets, heads := r.up.n("GET org/other"), r.up.n("HEAD org/other")
	if code, body, _ := r.pull("GET", bpath("org/other", sd), alice, "pw", nil); code != 200 || !bytes.Equal(body, shared) {
		t.Fatalf("%d", code)
	}
	if r.up.n("GET org/other") != gets {
		t.Fatalf("a layer that the repository has upstream is linked in, not downloaded: %d GETs", r.up.n("GET org/other")-gets)
	}
	if r.up.n("HEAD org/other") < heads+1 {
		t.Fatal("the link is made after a HEAD of that blob in that repository")
	}
}

// An object that is missing AFTER a valid proof is a 404 for that object and does not become a refusal of the repository.
func TestAMissingObjectDoesNotPoisonTheDecision(t *testing.T) {
	r := newRig(t)
	ld := r.up.put("org/private", "blobs", []byte("a real layer"))
	missing := "sha256:" + strings.Repeat("0", 64)
	if code, _, _ := r.pull("GET", bpath("org/private", ld), alice, "pw", nil); code != 200 { // the proof
		t.Fatalf("%d", code)
	}
	if code, _, _ := r.pull("GET", bpath("org/private", missing), alice, "pw", nil); code != 404 {
		t.Fatalf("a digest the repository does not have: %d", code)
	}
	if got := r.cache.Store.Lookup(DecisionKey{Hash: r.cache.hash("up", Creds{"alice", "pw"}), Repo: "up/org/private"}, "t1"); got != Serve {
		t.Fatalf("a 404 after the proof must not change the decision: %v", got)
	}
	// ... and a repository the credentials have no right to is refused (401)
	if code, _, _ := r.pull("GET", bpath("org/private", ld), mal, "pw", nil); code != 401 {
		t.Fatalf("a refusal stays a refusal: %d", code)
	}
}

func (r *rig) putTag(repo, tag string, body []byte) string {
	d := r.up.put(repo, "manifests", body)
	r.up.mu.Lock()
	r.up.objects["/v2/"+repo+"/manifests/"+tag] = body
	r.up.mu.Unlock()
	return d
}

func (r *rig) key(user string) DecisionKey {
	return DecisionKey{Hash: r.cache.hash("up", Creds{user, "pw"}), Repo: "up/org/private"}
}

// A 404 on a ref that the caller chose (an attacker's bogus tag) must not deny the repository for everybody who shares the credentials:
// it is answered 404, noted for that ref only, and a valid tag of the same hash is served right after.
func TestA404OnABogusTagDoesNotBlockAValidTag(t *testing.T) {
	r := newRig(t)
	r.putTag("org/private", "latest", []byte("manifest"))
	// first contact: nothing proven yet, so nothing is recorded at the repository level
	if code, _, _ := r.pull("GET", mpath("org/private", "bogus"), alice, "pw", nil); code != 404 {
		t.Fatalf("a bogus tag at first contact: %d", code)
	}
	if got := r.cache.Store.Lookup(r.key("alice"), "t1"); got != Check {
		t.Fatalf("a first-contact 404 records nothing at the repository level: %v", got)
	}
	if code, _, _ := r.pull("GET", mpath("org/private", "latest"), aliceT, "pw", nil); code != 200 {
		t.Fatalf("the valid tag of the same hash: %d", code)
	}
	// after a proof exists: the same bogus tag again (asked once, answered from the short negative) and another one
	if !r.cache.Store.Missing(MissKey{Hash: r.key("alice").Hash, Repo: "up/org/private", Kind: "manifests", Ref: "bogus"}) {
		t.Fatal("the ref is noted missing, for that ref only")
	}
	if code, _, _ := r.pull("GET", mpath("org/private", "bogus"), alice, "pw", nil); code != 404 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := r.pull("GET", mpath("org/private", "bogus2"), alice, "pw", nil); code != 404 {
		t.Fatalf("%d", code)
	}
	if got := r.cache.Store.Lookup(r.key("alice"), "t2"); got != Serve {
		t.Fatalf("the proven ref still answers 200, so the decision is untouched: %v", got)
	}
	if code, _, _ := r.pull("GET", mpath("org/private", "latest"), alice, "pw", nil); code != 200 {
		t.Fatalf("%d", code)
	}
	// the negative lapses
	r.clock.advance(time.Minute)
	if r.cache.Store.Missing(MissKey{Hash: r.key("alice").Hash, Repo: "up/org/private", Kind: "manifests", Ref: "bogus"}) {
		t.Fatal("a negative is kept for the denied window only")
	}
}

// The anonymous key never gets a repository-wide refusal from a 404.
func TestAnAnonymous404DoesNotBlockOthers(t *testing.T) {
	r := newRig(t)
	r.putTag("library/nginx", "latest", []byte("public manifest"))
	if code, _, _ := r.pull("GET", mpath("library/nginx", "bogus"), "", "", nil); code != 401 {
		t.Fatalf("an anonymous 404 is a 401 (the client comes back with credentials): %d", code)
	}
	if got := r.cache.Store.Lookup(DecisionKey{Repo: "up/library/nginx"}, ""); got == Refuse {
		t.Fatalf("no repository-wide refusal for the anonymous key: %v", got)
	}
	if code, _, _ := r.pull("GET", mpath("library/nginx", "latest"), "", "", nil); code != 200 {
		t.Fatalf("another anonymous pull of the repository: %d", code)
	}
}

// Revoked credentials: a ref that was proven answers 404 (a registry that hides the repository), and a 404 for another ref re-checks it,
// so the repository is denied for the key.
func TestRevokedCredentialsDenyTheRepositoryOnAKnownRef(t *testing.T) {
	r := newRig(t)
	r.putTag("org/private", "latest", []byte("manifest"))
	if code, _, _ := r.pull("GET", mpath("org/private", "latest"), alice, "pw", nil); code != 200 {
		t.Fatal(code)
	}
	r.up.mu.Lock()
	r.up.headStatus = http.StatusNotFound
	r.up.mu.Unlock()
	if code, _, _ := r.pull("GET", mpath("org/private", "bogus"), alice, "pw", nil); code != 404 {
		t.Fatalf("this request is not served: %d", code)
	}
	if got := r.cache.Store.Lookup(r.key("alice"), "t1"); got != Refuse {
		t.Fatalf("the proven ref answers 404 too: the repository is denied for the key: %v", got)
	}
}

// A background revalidation that gets 404 for the proof denies the key: access was revoked and the registry hides the repository.
func TestARevalidation404Denies(t *testing.T) {
	r := newRig(t)
	d := r.up.put("org/private", "manifests", []byte("m"))
	if code, _, _ := r.pull("GET", mpath("org/private", d), alice, "pw", nil); code != 200 {
		t.Fatal(code)
	}
	k := DecisionKey{Hash: r.cache.hash("up", Creds{"alice", "pw"}), Repo: "up/org/private"}
	r.clock.advance(3 * time.Minute) // past max-age, within stale-if-error: ServeStale
	r.up.mu.Lock()
	r.up.headStatus = http.StatusNotFound // the registry now hides the repository from these credentials
	r.up.mu.Unlock()
	if code, _, _ := r.pull("GET", mpath("org/private", d), alice, "pw", nil); code != 200 {
		t.Fatalf("the stale answer is served while it revalidates: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.cache.Store.Lookup(k, "t1") != Refuse && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := r.cache.Store.Lookup(k, "t1"); got != Refuse {
		t.Fatalf("a 404 on the revalidation denies the key: %v", got)
	}
}

// A blob HEAD that redirects is inconclusive: nothing is mounted, the blob is fetched through for this repository (and a fetch that the
// upstream refuses still fails there).
func TestARedirectedBlobProofMountsNothing(t *testing.T) {
	r := newRig(t)
	r.up.repos["org/other"] = map[string]bool{"alice": true}
	shared := bytes.Repeat([]byte("S"), 2048)
	sd := r.up.put("org/private", "blobs", shared)
	r.up.put("org/other", "blobs", shared)
	om := r.up.put("org/other", "manifests", []byte("m"))
	if code, _, _ := r.pull("GET", bpath("org/private", sd), alice, "pw", nil); code != 200 {
		t.Fatal(code)
	}
	if code, _, _ := r.pull("GET", mpath("org/other", om), alice, "pw", nil); code != 200 { // the decision for org/other, from a direct 200
		t.Fatal(code)
	}
	r.up.mu.Lock()
	r.up.redirect, r.up.redirectHeads = true, true
	r.up.mu.Unlock()
	gets := r.up.n("GET org/other")
	if code, body, _ := r.pull("GET", bpath("org/other", sd), alice, "pw", nil); code != 200 || !bytes.Equal(body, shared) {
		t.Fatalf("%d", code)
	}
	if r.up.n("GET org/other") == gets {
		t.Fatal("an inconclusive proof means the blob is downloaded for this repository, not mounted")
	}
}

// A public image that turned private must stop being served from the cache: the known ref of the anonymous record answers 404 now, a 404
// for any other ref re-checks it, and neither the denied window nor stale-if-error serves the old record afterwards.
func TestAPublicImageThatTurnedPrivateIsNoLongerServed(t *testing.T) {
	r := newRig(t)
	r.putTag("library/nginx", "latest", []byte("public manifest"))
	if code, _, _ := r.pull("GET", mpath("library/nginx", "latest"), "", "", nil); code != 200 {
		t.Fatalf("%d", code)
	}
	anon := DecisionKey{Repo: "up/library/nginx"}
	r.up.mu.Lock()
	r.up.headStatus = http.StatusNotFound // it turned private: the registry hides it
	r.up.mu.Unlock()
	if code, _, _ := r.pull("GET", mpath("library/nginx", "bogus"), "", "", nil); code == 200 {
		t.Fatalf("%d", code)
	}
	if got := r.cache.Store.Lookup(anon, ""); got != Refuse {
		t.Fatalf("the anonymous record is denied through its known ref: %v", got)
	}
	r.clock.advance(5 * time.Minute)
	r.up.setFail(503) // and the registry is down on top of it
	if code, _, _ := r.pull("GET", mpath("library/nginx", "latest"), "", "", nil); code == 200 {
		t.Fatalf("stale-if-error must not serve an image that was found private: %d", code)
	}
}

// An allowed record with no known ref cannot be re-checked: a 404 drops it (fail closed).
func TestAllowedWithoutAKnownRefIsDroppedOnA404(t *testing.T) {
	r := newRig(t)
	key := r.key("alice")
	r.cache.Store.Record(key, "t1", Allowed, r.cache.Store.Now())
	reg, _ := r.cache.Registries.Get("up")
	who := caller{tenant: "t1", creds: Creds{"alice", "pw"}, hash: key.Hash}
	r.cache.settleMissing(t.Context(), reg, key, who, Object{Repo: "org/private", Kind: "manifests", Ref: "bogus"})
	if ok, _, _ := r.cache.Store.Holds(key); ok {
		t.Fatal("the record is gone")
	}
	r.clock.advance(5 * time.Minute)
	r.up.setFail(503)
	if got := r.cache.Store.Lookup(key, "t1"); got != Check {
		t.Fatalf("no stale answer from a dropped record: %v", got)
	}
}

// The tag path that records an allow but gets no digest (502) still leaves a known ref behind.
func TestATagWithoutADigestStillLeavesAKnownRef(t *testing.T) {
	r := newRig(t)
	r.putTag("org/private", "latest", []byte("manifest"))
	r.up.mu.Lock()
	r.up.noDigestHeader = true
	r.up.mu.Unlock()
	if code, _, _ := r.pull("GET", mpath("org/private", "latest"), alice, "pw", nil); code != 502 {
		t.Fatalf("%d", code)
	}
	key := r.key("alice")
	ok, kg, has := r.cache.Store.Holds(key)
	if ok && (!has || kg.Ref != "latest") {
		t.Fatalf("an allow without a known ref: %v %+v", has, kg)
	}
}
