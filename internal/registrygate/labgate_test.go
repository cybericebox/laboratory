package registrygate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/names"
)

// fakeZot answers what the push path needs and records every request that reaches it.
type fakeZot struct {
	mu        sync.Mutex
	reqs      []zotReq
	next      int
	latest    string // Docker-Content-Digest of "latest" before a push
	putDigest string
	fail      []int // statuses to answer with, once each, in place of the normal answer
}

type zotReq struct {
	method, path, query string
	auth                string
	cibHeaders          int
	body                int64
}

func (z *fakeZot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n, _ := io.Copy(io.Discard, r.Body)
	cib := 0
	for k := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), HeaderPrefix) {
			cib++
		}
	}
	z.mu.Lock()
	z.reqs = append(z.reqs, zotReq{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), cib, n})
	if len(z.fail) > 0 {
		code := z.fail[0]
		z.fail = z.fail[1:]
		z.mu.Unlock()
		w.WriteHeader(code)
		return
	}
	z.next++
	id := fmt.Sprintf("sess-%d", z.next)
	latest, put := z.latest, z.putDigest
	z.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/blobs/uploads"):
		if r.URL.Query().Get("mount") != "" {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Location", strings.TrimSuffix(r.URL.Path, "/")+"/"+id)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPatch:
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/"):
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/"):
		w.Header().Set("Docker-Content-Digest", put)
		w.WriteHeader(http.StatusCreated)
	case (r.Method == http.MethodHead || r.Method == http.MethodGet) && strings.HasSuffix(r.URL.Path, "/manifests/latest") && latest != "":
		w.Header().Set("Docker-Content-Digest", latest)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (z *fakeZot) count(method, pathPart string) int {
	z.mu.Lock()
	defer z.mu.Unlock()
	c := 0
	for _, r := range z.reqs {
		if r.method == method && strings.Contains(r.path, pathPart) {
			c++
		}
	}
	return c
}

// list is a copy of what zot has received so far; the handlers of a cut request may still be finishing, so it settles for a moment first.
func (z *fakeZot) list() []zotReq {
	time.Sleep(100 * time.Millisecond)
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]zotReq(nil), z.reqs...)
}

func (z *fakeZot) failNext(code int) {
	z.mu.Lock()
	z.fail = append(z.fail, code)
	z.mu.Unlock()
}

func (z *fakeZot) total() int {
	z.mu.Lock()
	defer z.mu.Unlock()
	return len(z.reqs)
}

// memStore is the snapshot repositories as the gate reads and deletes them.
type memStore struct {
	mu        sync.Mutex
	missing   map[string]bool   // digests the registry does not hold
	blobs     map[string]int64  // repo|digest -> size as the registry holds it
	manifests map[string][]byte // repo|ref -> body
	deleted   []string
}

func (m *memStore) Head(_ context.Context, repo, kind, ref string) (http.Header, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if kind == kindBlobs {
		n, ok := m.blobs[repo+"|"+ref]
		if !ok && !m.missing[ref] {
			n, ok = 2, true // the config blob every manifest of the tests lists
		}
		if !ok {
			return nil, false, nil
		}
		h := http.Header{}
		h.Set("Content-Length", strconv.FormatInt(n, 10))
		return h, true, nil
	}
	b, ok := m.manifests[repo+"|"+ref]
	if !ok {
		return nil, false, nil
	}
	h := http.Header{}
	h.Set("Docker-Content-Digest", sha256Digest(b))
	return h, true, nil
}

func (m *memStore) ManifestBody(_ context.Context, repo, ref string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.manifests[repo+"|"+ref]; ok {
		return b, nil
	}
	for k, b := range m.manifests { // by digest
		if strings.HasPrefix(k, repo+"|") && sha256Digest(b) == ref {
			return b, nil
		}
	}
	return nil, nil
}

func (m *memStore) DeleteManifest(_ context.Context, repo, digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, "manifest "+repo+" "+digest)
	return nil
}

func (m *memStore) DeleteBlob(_ context.Context, repo, digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, "blob "+repo+" "+digest)
	return nil
}

func (m *memStore) Repos(context.Context) ([]string, error)        { return nil, nil }
func (m *memStore) Tags(context.Context, string) ([]string, error) { return nil, nil }

type fakeMeter struct {
	blobs map[string]int64
	err   error
	asked []string
}

func (f *fakeMeter) TenantBlobs(_ context.Context, tenant, except string) (map[string]int64, error) {
	f.asked = append(f.asked, tenant+"|"+except)
	return f.blobs, f.err
}

type labRig struct {
	t      *testing.T
	zot    *fakeZot
	zotSrv *httptest.Server
	gate   *Gate
	lab    *LabGate
	sch    *Schedule
	store  *memStore
	meter  *fakeMeter
	sup    *Superseder
	clk    *time.Time
	free   float64
}

const repoD1 = "lab/lg-acme/lab1/dev1"

func newLabRig(t *testing.T) *labRig {
	t.Helper()
	r := &labRig{t: t, zot: &fakeZot{}, store: &memStore{manifests: map[string][]byte{}, blobs: map[string]int64{}, missing: map[string]bool{}}, meter: &fakeMeter{blobs: map[string]int64{}}, free: 0.5}
	r.zotSrv = httptest.NewServer(r.zot)
	t.Cleanup(r.zotSrv.Close)
	clk := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	r.clk = &clk
	now := func() time.Time { return *r.clk }
	r.sch = NewSchedule()
	r.sch.Now = now
	r.sch.Confirm()
	r.lab = NewLabGate(mustURL(t, r.zotSrv.URL), "gate", "gatepw")
	r.sup = &Superseder{Store: r.store, Grace: 2 * time.Minute, Now: now}
	r.lab.Schedule, r.lab.Store, r.lab.Super, r.lab.Meter, r.lab.Now = r.sch, r.store, r.sup, r.meter, now
	r.lab.Free = func() float64 { return r.free }
	r.lab.Limits = PushLimits{MinInterval: time.Minute, Budget: 1000, BudgetWindow: time.Hour, Reserve: 0.1, TenantQuota: 10000}
	g, err := New(strings.TrimPrefix(r.zotSrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	g.Auth = &Authenticator{Reviewer: testReviewer{}, Accounts: testAccounts, Schedule: r.sch, Now: now, Precheck: anyShape}
	g.Lab = r.lab
	r.gate = g
	// the cluster: two nodes, the devices of one lab
	r.sch.SetGroup("lg-acme", "acme")
	r.sch.SetGroup("lg-other", "other")
	r.sch.SetPod(PodInfo{Namespace: "laboratory-system", Name: "agent-a", UID: "uid-a", Node: "node-a", Phase: "Running"})
	r.sch.SetPod(PodInfo{Namespace: "laboratory-system", Name: "agent-b", UID: "uid-b", Node: "node-b", Phase: "Running"})
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "dev1", Node: "node-a", StateEnabled: true, Tenant: "acme"})
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "dev2", Node: "node-b", StateEnabled: true, Tenant: "acme"})
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "plain", Node: "node-a", StateEnabled: false, Tenant: "acme"})
	return r
}

func (r *labRig) do(token, method, target string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://x"+target, body)
	req.Header.Set(HeaderNodeToken, token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.gate.ServeHTTP(w, req)
	return w
}

func upPath(repo string) string       { return "/v2/" + repo + "/blobs/uploads/" }
func manPath(repo, ref string) string { return "/v2/" + repo + "/manifests/" + ref }

func snapshotManifest(layers map[string]int64) []byte {
	var ls []string
	for d, n := range layers {
		ls = append(ls, fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}`, d, n))
	}
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":"sha256:%s","size":2},"layers":[%s]}`, strings.Repeat("c", 64), strings.Join(ls, ",")))
}

func dg(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// 2.4.3, rows for lab/**: reads, writes and deletes of a node, the operator and the agent.
func TestLabDecisionTable(t *testing.T) {
	r := newLabRig(t)
	body := func() io.Reader { return bytes.NewReader(snapshotManifest(nil)) }
	type row struct {
		name   string
		token  string
		method string
		target string
		body   bool
		want   int
	}
	rows := []row{
		{"node reads the manifest of its device", tokA, "GET", manPath(repoD1, "latest"), false, 200},
		{"node HEADs a blob of its device", tokA, "HEAD", "/v2/" + repoD1 + "/blobs/" + dg("a"), false, 200},
		{"node reads the repository of a device on another node", tokA, "GET", manPath("lab/lg-acme/lab1/dev2", "latest"), false, 403},
		{"the other node reads its own", tokB, "GET", manPath("lab/lg-acme/lab1/dev2", "latest"), false, 200},
		{"a device that is not snapshot-backed", tokA, "GET", manPath("lab/lg-acme/lab1/plain", "latest"), false, 403},
		{"a device nobody knows", tokA, "GET", manPath("lab/lg-acme/lab1/ghost", "latest"), false, 403},
		{"a lab of another namespace", tokA, "GET", manPath("lab/lg-other/lab1/dev1", "latest"), false, 403},
		{"the shared base repository", tokA, "GET", "/v2/base/manifests/latest", false, 403},
		{"node starts an upload for its device", tokA, "POST", upPath(repoD1), false, 202},
		{"node starts an upload for a device on another node", tokA, "POST", upPath("lab/lg-acme/lab1/dev2"), false, 403},
		{"node starts an upload for a plain device", tokA, "POST", upPath("lab/lg-acme/lab1/plain"), false, 403},
		{"node writes a manifest of its device", tokA, "PUT", manPath(repoD1, "latest"), true, 201},
		{"node writes a manifest under a digest", tokA, "PUT", manPath(repoD1, dg("a")), true, 403},
		{"node writes under another tag", tokA, "PUT", manPath(repoD1, "v2"), true, 403},
		{"node deletes a manifest", tokA, "DELETE", manPath(repoD1, dg("a")), false, 403},
		{"node deletes a blob", tokA, "DELETE", "/v2/" + repoD1 + "/blobs/" + dg("a"), false, 403},
		{"node deletes an upload session", tokA, "DELETE", upPath(repoD1) + "sess-1", false, 403},
		{"node reads the catalog", tokA, "GET", "/v2/_catalog", false, 403},
		{"node lists tags", tokA, "GET", "/v2/" + repoD1 + "/tags/list", false, 403},
		{"operator reads any lab repository", "operator-token", "GET", manPath("lab/lg-acme/lab1/dev2", "latest"), false, 200},
		{"operator deletes a manifest", "operator-token", "DELETE", manPath(repoD1, dg("a")), false, 200},
		{"operator deletes a blob", "operator-token", "DELETE", "/v2/" + repoD1 + "/blobs/" + dg("a"), false, 200},
		{"operator reads the catalog", "operator-token", "GET", "/v2/_catalog", false, 200},
		{"operator lists tags", "operator-token", "GET", "/v2/" + repoD1 + "/tags/list", false, 200},
		{"operator reads base", "operator-token", "GET", "/v2/base/manifests/latest", false, 200},
		{"operator writes a manifest", "operator-token", "PUT", manPath(repoD1, "latest"), true, 403},
		{"operator starts an upload", "operator-token", "POST", upPath(repoD1), false, 403},
		{"operator deletes in base", "operator-token", "DELETE", "/v2/base/manifests/" + dg("a"), false, 403},
		{"agent reads any lab repository", "agent-token", "GET", manPath(repoD1, "latest"), false, 200},
		{"agent reads the catalog", "agent-token", "GET", "/v2/_catalog", false, 403},
		{"agent writes", "agent-token", "PUT", manPath(repoD1, "latest"), true, 403},
		{"agent deletes", "agent-token", "DELETE", manPath(repoD1, dg("a")), false, 403},
		{"agent reads base", "agent-token", "GET", "/v2/base/manifests/latest", false, 403},
	}
	for _, c := range rows {
		// fresh limits for every row: the rows are about who may, not about how much
		r.lab.mu.Lock()
		r.lab.state = map[string]*pushState{}
		r.lab.mu.Unlock()
		var b io.Reader
		if c.body {
			b = body()
		}
		w := r.do(c.token, c.method, c.target, b, nil)
		if w.Code != c.want {
			t.Errorf("%s: %s %s -> %d, want %d (%s)", c.name, c.method, c.target, w.Code, c.want, strings.TrimSpace(w.Body.String()))
		}
	}
}

// A refused request never reaches zot, and what reaches zot carries the gate's account and no X-Cib-* header.
func TestOnlyCheckedRequestsReachZotWithTheGatesAccount(t *testing.T) {
	r := newLabRig(t)
	if w := r.do(tokA, "DELETE", manPath(repoD1, dg("a")), nil, nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := r.do(tokB, "GET", manPath(repoD1, "latest"), nil, nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if r.zot.total() != 0 {
		t.Fatalf("a refused request reached zot: %d", r.zot.total())
	}
	w := r.do(tokA, "GET", manPath(repoD1, "latest"), nil, map[string]string{"X-Cib-Node": "node-b", "Authorization": "Basic ZXZpbDpldmls"})
	if w.Code != 200 || r.zot.total() != 1 {
		t.Fatalf("%d %d", w.Code, r.zot.total())
	}
	zr := r.zot.list()[0]
	if zr.cibHeaders != 0 || !strings.HasPrefix(zr.auth, "Basic ") || zr.auth == "Basic ZXZpbDpldmls" {
		t.Fatalf("zot saw %+v", zr)
	}
}

// An upload session belongs to the node and the repository that started it.
func TestUploadSessionsAreBoundToTheNodeAndTheRepository(t *testing.T) {
	r := newLabRig(t)
	w := r.do(tokA, "POST", upPath(repoD1), nil, nil)
	loc := w.Header().Get("Location")
	if w.Code != 202 || !strings.Contains(loc, "/uploads/sess-") {
		t.Fatalf("start: %d %q", w.Code, loc)
	}
	sess := loc[strings.LastIndex(loc, "/")+1:]
	patch := func(token, repo, id string) int {
		return r.do(token, "PATCH", "/v2/"+repo+"/blobs/uploads/"+id, strings.NewReader("12345"), nil).Code
	}
	if c := patch(tokA, repoD1, sess); c != 202 {
		t.Fatalf("the node that started it: %d", c)
	}
	if c := patch(tokB, "lab/lg-acme/lab1/dev2", sess); c != 404 {
		t.Fatalf("another node, its own repository, someone else's session: %d", c)
	}
	// the session id of dev1 used on dev1's path by another node is refused by the device rule first; on dev2's path by its node it is unknown
	if c := patch(tokA, repoD1, "sess-unknown"); c != 404 {
		t.Fatalf("an unknown session: %d", c)
	}
	if c := r.do(tokA, "GET", "/v2/"+repoD1+"/blobs/uploads/sess-unknown", nil, nil).Code; c != 404 {
		t.Fatalf("an unknown session on GET: %d", c)
	}
	// the device leaves the node: the session is refused although the repository is the same
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "dev1", Node: "node-b", StateEnabled: true, Tenant: "acme"})
	if c := patch(tokA, repoD1, sess); c != 404 {
		t.Fatalf("the device is no longer on the node: the session is unknown: %d", c)
	}
	if r.zot.count("DELETE", "/blobs/uploads/"+sess) != 1 {
		t.Fatal("zot must be told to cancel the session")
	}
	// finishing a session ends it
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "dev1", Node: "node-a", StateEnabled: true, Tenant: "acme"})
	w2 := r.do(tokA, "POST", upPath(repoD1), nil, nil)
	loc2 := w2.Header().Get("Location")
	sess = loc2[strings.LastIndex(loc2, "/")+1:]
	if c := r.do(tokA, "PUT", "/v2/"+repoD1+"/blobs/uploads/"+sess+"?digest="+dg("a"), strings.NewReader("1"), nil).Code; c != 201 {
		t.Fatalf("commit: %d", c)
	}
	if c := patch(tokA, repoD1, sess); c != 404 {
		t.Fatalf("a finished session: %d", c)
	}
	// a gate restart forgets sessions: an in-flight upload fails and the node-state retries
	r.lab.mu.Lock()
	r.lab.sess = map[string]uploadSession{}
	r.lab.mu.Unlock()
	if c := patch(tokA, repoD1, sess); c != 404 {
		t.Fatalf("after a restart: %d", c)
	}
}

// Review finding 2 / 2.4.3: the gate computes the mount source itself, from the image of that very device on the calling node, bound to the
// device's tenant, and only a digest that image's live proven grant covers can be mounted; nothing is ever mounted from base.
func TestBlobMountsOnlyFromTheDevicesOwnProvenImage(t *testing.T) {
	r := newLabRig(t)
	now := func() time.Time { return *r.clk }
	r.lab.Cache = &Cache{Grants: &Grants{Now: now}, Schedule: r.sch}
	devPod := func(name, ns, dev, node, image string) {
		r.sch.SetPod(PodInfo{Namespace: ns, Name: name, UID: "u-" + name, Node: node, Phase: "Running", Images: []string{image},
			Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: dev}})
	}
	const nginx = "docker.io/library/nginx"
	devPod("p1", "lg-acme", "dev1", "node-a", "localhost:5035/c/acme/"+nginx+"@"+dg("1")) // pinned: the index dg(1) lists child dg(2)
	devPod("p2", "lg-acme", "plain", "node-a", "localhost:5035/c/acme/docker.io/library/redis:7")
	devPod("p3", "lg-acme", "dev2", "node-b", "localhost:5035/c/acme/"+nginx+"@"+dg("1"))
	g := r.lab.Cache.Grants
	add := func(node, seg, repo, tag, root string, kids ...string) {
		t.Helper()
		if err := g.Add(node, seg, repo, Creds{}, tag, root, kids); err != nil {
			t.Fatal(err)
		}
	}
	add("node-a", "acme", nginx, "", dg("1"), dg("2"))
	add("node-a", "acme", nginx, "", dg("2"), dg("3"), dg("4")) // the child manifest lists layers 3 and 4
	add("node-a", "acme", nginx, "", dg("5"), dg("6"))          // ANOTHER image of the same repository (some other pod's), layer 6
	add("node-a", "acme", "docker.io/library/redis", "7", dg("7"), dg("8"))
	add("node-a", "other", "docker.io/library/secret", "", dg("9"), dg("a")) // another tenant's cached image on the node
	add("node-b", "acme", nginx, "", dg("1"), dg("2"))
	mount := func(token, repo, digest, from string) int {
		q := "?mount=" + digest
		if from != "" {
			q += "&from=" + from
		}
		return r.do(token, "POST", upPath(repo)+q, nil, nil).Code
	}
	for _, c := range []struct {
		name, token, repo, digest, from string
		want                            int
	}{
		{"a layer of the pinned image", tokA, repoD1, dg("3"), "", 201},
		{"the pinned index itself", tokA, repoD1, dg("1"), "", 201},
		{"the caller's from is ignored", tokA, repoD1, dg("4"), "base", 201},
		{"a digest granted for ANOTHER image of the same repository", tokA, repoD1, dg("6"), "", 403},
		{"the same, with a from naming that repository", tokA, repoD1, dg("6"), "c/" + nginx, 403},
		{"a digest no grant holds", tokA, repoD1, dg("b"), "", 403},
		{"an image of another device of the lab", tokA, repoD1, dg("8"), "c/docker.io/library/redis", 403},
		{"another tenant's cached image", tokA, repoD1, dg("a"), "c/docker.io/library/secret", 403},
		{"the same layer on the device of another node", tokB, "lab/lg-acme/lab1/dev2", dg("3"), "", 403}, // node-b holds no grant of dg(3)
		{"no digest", tokA, repoD1, "", "c/" + nginx, 202},                                                // not a mount: an ordinary upload start
	} {
		want := c.want
		if got := mount(c.token, c.repo, c.digest, c.from); got != want {
			t.Errorf("%s: %d, want %d", c.name, got, want)
		}
	}
	// the source the gate sent to zot is the one it computed
	for _, z := range r.zot.list() {
		if strings.Contains(z.query, "mount=") && !strings.Contains(z.query, "from=c%2Fdocker.io%2Flibrary%2Fnginx") {
			t.Errorf("zot got %q: the gate must name the source itself", z.query)
		}
		if strings.Contains(z.query, "base") {
			t.Errorf("a mount from base reached zot: %q", z.query)
		}
	}
	// a device pod whose image string carries ANOTHER tenant's segment gets nothing, nor does the platform segment
	devPod("p4", "lg-acme", "dev1", "node-a", "localhost:5035/c/other/docker.io/library/secret@"+dg("9"))
	if got := mount(tokA, repoD1, dg("a"), ""); got != 403 {
		t.Errorf("a pod of tenant acme naming tenant other's image: %d", got)
	}
	add("node-a", "platform", "docker.io/library/vpn", "", dg("c"), dg("d"))
	devPod("p5", "lg-acme", "dev1", "node-a", "localhost:5035/c/platform/docker.io/library/vpn@"+dg("c"))
	if got := mount(tokA, repoD1, dg("d"), ""); got != 403 {
		t.Errorf("the platform segment: %d", got)
	}
	// a pod in a namespace of another tenant's group does not count for this device
	devPod("p6", "lg-other", "dev1", "node-a", "localhost:5035/c/acme/"+nginx+"@"+dg("1"))
	r.sch.DeletePod("lg-acme", "p1")
	if got := mount(tokA, repoD1, dg("3"), ""); got != 403 {
		t.Errorf("no pod of this device names the image any more: %d", got)
	}
	// a tag pod: the tag covers what it was served as
	devPod("p7", "lg-acme", "dev1", "node-a", "localhost:5035/c/acme/docker.io/library/redis:7")
	if got := mount(tokA, repoD1, dg("8"), ""); got != 201 {
		t.Errorf("a tag reference covers its manifest's layers: %d", got)
	}
	// a finished pod's image opens nothing
	r.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "p7", UID: "u-p7", Node: "node-a", Phase: "Failed", Images: []string{"localhost:5035/c/acme/docker.io/library/redis:7"},
		Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}})
	if got := mount(tokA, repoD1, dg("8"), ""); got != 403 {
		t.Errorf("a finished pod: %d", got)
	}
}

// An upload into the device repository of a digest that is in the grant is the image's layer (not budgeted); any other digest is the
// snapshot's own layer and counts.
func TestUploadOfAGrantedDigestIsNotBudgeted(t *testing.T) {
	r := newLabRig(t) // budget 1000
	r.lab.Cache = &Cache{Grants: &Grants{Now: func() time.Time { return *r.clk }}, Schedule: r.sch}
	r.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "p1", UID: "u1", Node: "node-a", Phase: "Running", Images: []string{"localhost:5035/c/acme/docker.io/library/x@" + dg("1")},
		Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}})
	if err := r.lab.Cache.Grants.Add("node-a", "acme", "docker.io/library/x", Creds{}, "", dg("1"), []string{dg("2")}); err != nil {
		t.Fatal(err)
	}
	w := r.do(tokA, "POST", upPath(repoD1), nil, nil)
	loc := w.Header().Get("Location")
	s1 := "/v2/" + repoD1 + "/blobs/uploads/" + loc[strings.LastIndex(loc, "/")+1:]
	if w := r.do(tokA, "PUT", s1+"?digest="+dg("2"), bytes.NewReader(make([]byte, 5000)), nil); w.Code != 201 {
		t.Fatalf("the image's own layer: %d", w.Code)
	}
	w = r.do(tokA, "POST", upPath(repoD1), nil, nil)
	loc = w.Header().Get("Location")
	s2 := "/v2/" + repoD1 + "/blobs/uploads/" + loc[strings.LastIndex(loc, "/")+1:]
	if w := r.do(tokA, "PUT", s2+"?digest="+dg("9"), bytes.NewReader(make([]byte, 5000)), nil); w.Code != 429 {
		t.Fatalf("the snapshot's own layer over the budget: %d", w.Code)
	}
}

// 2.4.4: the minimum interval between two accepted snapshots, with Retry-After; the exit snapshot of an ended pod is not held back, once.
func TestMinIntervalAndTheExitSnapshot(t *testing.T) {
	r := newLabRig(t)
	r.zot.putDigest = dg("1")
	put := func() *httptest.ResponseRecorder {
		return r.do(tokA, "PUT", manPath(repoD1, "latest"), bytes.NewReader(snapshotManifest(nil)), nil)
	}
	if w := put(); w.Code != 201 {
		t.Fatalf("first: %d", w.Code)
	}
	*r.clk = r.clk.Add(20 * time.Second)
	w := put()
	if w.Code != 429 || w.Header().Get("Retry-After") != "41" || !strings.Contains(w.Body.String(), CodePushInterval) {
		t.Fatalf("too soon: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	*r.clk = r.clk.Add(41 * time.Second)
	if w := put(); w.Code != 201 {
		t.Fatalf("after the interval: %d", w.Code)
	}
	// the pod of the device ends on the node: its exit snapshot goes through at once, once per pod UID
	pod := PodInfo{Namespace: "lg-acme", Name: "dev1-0", UID: "pod-1", Node: "node-a", Phase: "Running",
		Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}}
	r.sch.SetPod(pod)
	*r.clk = r.clk.Add(time.Second)
	if w := put(); w.Code != 429 {
		t.Fatalf("a pod that is still running has no exit snapshot: %d", w.Code)
	}
	pod.Phase = "Succeeded"
	r.sch.SetPod(pod)
	if w := put(); w.Code != 201 {
		t.Fatalf("the exit snapshot: %d", w.Code)
	}
	if w := put(); w.Code != 201 {
		t.Fatalf("a device whose pod stays ended is held to the per-hour exit limit, not to the interval: %d", w.Code)
	}
	r.sch.DeletePod("lg-acme", "dev1-0")
	// a pod of another device, or on another node, is no exit of this one
	r.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "x", UID: "pod-3", Node: "node-b", Phase: "Failed", Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}})
	r.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "y", UID: "pod-4", Node: "node-a", Phase: "Failed", Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev2"}})
	if w := put(); w.Code != 429 {
		t.Fatalf("an ended pod elsewhere or of another device: %d", w.Code)
	}
}

// 2.4.4: the push budget in a sliding window: a request that cannot fit is refused at once, a body is cut at the line, and the window moves.
func TestPushBudgetSlidingWindow(t *testing.T) {
	r := newLabRig(t)
	start := func() string {
		w := r.do(tokA, "POST", upPath(repoD1), nil, nil)
		loc := w.Header().Get("Location")
		return "/v2/" + repoD1 + "/blobs/uploads/" + loc[strings.LastIndex(loc, "/")+1:]
	}
	s1 := start()
	if w := r.do(tokA, "PATCH", s1, bytes.NewReader(make([]byte, 600)), nil); w.Code != 202 {
		t.Fatalf("600 of 1000: %d", w.Code)
	}
	if got := func() int64 { l := r.zot.list(); return l[len(l)-1].body }(); got != 600 {
		t.Fatalf("zot got %d bytes", got)
	}
	w := r.do(tokA, "PATCH", s1, bytes.NewReader(make([]byte, 600)), nil)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), CodePushBudget) {
		t.Fatalf("600 more: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	// an upload with no length is cut at the line: zot never receives more than what is left, and nothing beyond the budget is charged
	before := len(r.zot.list())
	req := httptest.NewRequest("PATCH", "http://x"+s1, io.NopCloser(bytes.NewReader(make([]byte, 5000))))
	req.ContentLength = -1
	req.Header.Set(HeaderNodeToken, tokA)
	r.gate.ServeHTTP(httptest.NewRecorder(), req)
	for _, z := range r.zot.list()[before:] {
		if z.body > 400 {
			t.Fatalf("the body was not cut at the budget: %+v", z)
		}
	}
	if used, _ := r.lab.used(repoD1, false, *r.clk); used > 1000 {
		t.Fatalf("charged %d of a budget of 1000", used)
	}
	// the window moves on: an hour later the whole budget is available again
	*r.clk = r.clk.Add(time.Hour + time.Second)
	r.sch.Confirm()
	if w := r.do(tokA, "PATCH", s1, bytes.NewReader(make([]byte, 900)), nil); w.Code != 202 {
		t.Fatalf("a new window: %d", w.Code)
	}
	// a budget of zero is no budget
	r.lab.Limits.Budget = 0
	if w := r.do(tokA, "PATCH", s1, bytes.NewReader(make([]byte, 5000)), nil); w.Code != 202 {
		t.Fatalf("no budget: %d", w.Code)
	}
}

// The budget is per device repository, and an exit snapshot is not held back by it.
func TestPushBudgetIsPerDeviceAndTheExitSnapshotHasAPoolOfItsOwn(t *testing.T) {
	r := newLabRig(t)
	sess := func(token, repo string) string {
		w := r.do(token, "POST", upPath(repo), nil, nil)
		loc := w.Header().Get("Location")
		return "/v2/" + repo + "/blobs/uploads/" + loc[strings.LastIndex(loc, "/")+1:]
	}
	a, b := sess(tokA, repoD1), sess(tokB, "lab/lg-acme/lab1/dev2")
	if w := r.do(tokA, "PATCH", a, bytes.NewReader(make([]byte, 900)), nil); w.Code != 202 {
		t.Fatal(w.Code)
	}
	if w := r.do(tokB, "PATCH", b, bytes.NewReader(make([]byte, 900)), nil); w.Code != 202 {
		t.Fatalf("another device has a budget of its own: %d", w.Code)
	}
	if w := r.do(tokA, "PATCH", a, bytes.NewReader(make([]byte, 900)), nil); w.Code != 429 {
		t.Fatalf("over the budget: %d", w.Code)
	}
	r.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "dev1-0", UID: "pod-1", Node: "node-a", Phase: "Failed", Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}})
	if w := r.do(tokA, "PATCH", a, bytes.NewReader(make([]byte, 900)), nil); w.Code != 202 {
		t.Fatalf("the exit snapshot is not held back by the budget the running pod used: %d", w.Code)
	}
	if w := r.do(tokA, "PATCH", a, bytes.NewReader(make([]byte, 900)), nil); w.Code != 429 {
		t.Fatalf("but the exit snapshot is bounded too: %d", w.Code)
	}
}

// Review finding 5 / 2.4.4: exit pushes are limited per device per hour whatever the pod UIDs, and one exit push has a ceiling.
func TestExitPushesAreLimitedPerDeviceNotPerPod(t *testing.T) {
	r := newLabRig(t)
	r.zot.putDigest = dg("1")
	crash := func(i int) {
		r.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: fmt.Sprintf("dev1-%d", i), UID: fmt.Sprintf("pod-%d", i), Node: "node-a", Phase: "Failed",
			Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}})
	}
	put := func() *httptest.ResponseRecorder {
		return r.do(tokA, "PUT", manPath(repoD1, "latest"), bytes.NewReader(snapshotManifest(nil)), nil)
	}
	for i := 1; i <= 6; i++ {
		crash(i) // a new pod UID every time: no help
		if w := put(); w.Code != 201 {
			t.Fatalf("exit %d: %d", i, w.Code)
		}
	}
	crash(7)
	w := put()
	if w.Code != 429 || !strings.Contains(w.Body.String(), CodeExitLimit) || w.Header().Get("Retry-After") == "" {
		t.Fatalf("the seventh in the hour: %d %s", w.Code, w.Body.String())
	}
	// uploads of that exit push are refused too, not only the manifest
	if w := r.do(tokA, "POST", upPath(repoD1), nil, nil); w.Code != 429 || !strings.Contains(w.Body.String(), CodeExitLimit) {
		t.Fatalf("an upload start: %d %s", w.Code, w.Body.String())
	}
	*r.clk = r.clk.Add(time.Hour + time.Minute)
	r.sch.Confirm()
	if w := put(); w.Code != 201 {
		t.Fatalf("a new hour: %d", w.Code)
	}
}

func TestAnExitPushHasACeilingOfItsOwn(t *testing.T) {
	r := newLabRig(t) // budget 1000
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "dev1", Node: "node-a", StateEnabled: true, Tenant: "acme", WriteQuotaBytes: 300})
	r.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "dev1-0", UID: "pod-1", Node: "node-a", Phase: "Failed", Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}})
	w := r.do(tokA, "POST", upPath(repoD1), nil, nil)
	loc := w.Header().Get("Location")
	s1 := "/v2/" + repoD1 + "/blobs/uploads/" + loc[strings.LastIndex(loc, "/")+1:]
	w = r.do(tokA, "PATCH", s1, bytes.NewReader(make([]byte, 500)), nil)
	if w.Code != 429 || !strings.Contains(w.Body.String(), CodeExitLimit) {
		t.Fatalf("500 against the device's write quota of 300: %d %s", w.Code, w.Body.String())
	}
	if w := r.do(tokA, "PATCH", s1, bytes.NewReader(make([]byte, 300)), nil); w.Code != 202 {
		t.Fatalf("300: %d", w.Code)
	}
	r.lab.Limits.ExitMaxBytes = 1000 // the explicit setting beats the device's quota
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "dev1", Node: "node-a", StateEnabled: true, Tenant: "acme", WriteQuotaBytes: 300})
	if w := r.do(tokA, "PATCH", s1, bytes.NewReader(make([]byte, 600)), nil); w.Code != 202 {
		t.Fatalf("GATE_EXIT_MAX_BYTES: %d", w.Code)
	}
}

// 2.4.4: a request zot refuses gives its reservation back.
func TestAReservationIsReturnedWhenZotRefuses(t *testing.T) {
	r := newLabRig(t) // budget 1000
	w := r.do(tokA, "POST", upPath(repoD1), nil, nil)
	loc := w.Header().Get("Location")
	s1 := "/v2/" + repoD1 + "/blobs/uploads/" + loc[strings.LastIndex(loc, "/")+1:]
	r.zot.failNext(http.StatusBadRequest)
	if w := r.do(tokA, "PATCH", s1, bytes.NewReader(make([]byte, 800)), nil); w.Code != 400 {
		t.Fatalf("zot refuses: %d", w.Code)
	}
	if used, _ := r.lab.used(repoD1, false, *r.clk); used != 0 {
		t.Fatalf("%d bytes stay charged for a request that stored nothing", used)
	}
	if w := r.do(tokA, "PATCH", s1, bytes.NewReader(make([]byte, 800)), nil); w.Code != 202 {
		t.Fatalf("the budget is whole again: %d", w.Code)
	}
}

// Review finding 6: concurrent uploads share what is left of the budget; they never each get all of it.
func TestConcurrentUploadsShareTheBudget(t *testing.T) {
	r := newLabRig(t) // budget 1000
	sess := func() string {
		w := r.do(tokA, "POST", upPath(repoD1), nil, nil)
		loc := w.Header().Get("Location")
		return "/v2/" + repoD1 + "/blobs/uploads/" + loc[strings.LastIndex(loc, "/")+1:]
	}
	s1, s2 := sess(), sess()
	var wg sync.WaitGroup
	for _, sp := range []string{s1, s2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("PATCH", "http://x"+sp, io.NopCloser(bytes.NewReader(make([]byte, 800))))
			req.ContentLength = -1
			req.Header.Set(HeaderNodeToken, tokA)
			r.gate.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()
	var total int64
	for _, z := range r.zot.list() {
		if z.method == "PATCH" {
			total += z.body
		}
	}
	if total > 1000 {
		t.Fatalf("zot received %d bytes of a budget of 1000", total)
	}
}

// Review finding 6: a monolithic upload (POST with the body) is budgeted like a PATCH.
func TestMonolithicUploadIsBudgeted(t *testing.T) {
	r := newLabRig(t)
	w := r.do(tokA, "POST", upPath(repoD1)+"?digest="+dg("a"), bytes.NewReader(make([]byte, 2000)), nil)
	if w.Code != 429 || !strings.Contains(w.Body.String(), CodePushBudget) {
		t.Fatalf("2000 of 1000: %d %s", w.Code, w.Body.String())
	}
	if w := r.do(tokA, "POST", upPath(repoD1)+"?digest="+dg("a"), bytes.NewReader(make([]byte, 600)), nil); w.Code != 202 {
		t.Fatalf("600 of 1000: %d", w.Code)
	}
	if w := r.do(tokA, "POST", upPath(repoD1)+"?digest="+dg("a"), bytes.NewReader(make([]byte, 600)), nil); w.Code != 429 {
		t.Fatalf("the second 600: %d", w.Code)
	}
}

// Review finding 6: the quota counts the sizes the registry holds, not the ones a node wrote in its manifest.
func TestQuotaUsesTheRealBlobSizes(t *testing.T) {
	r := newLabRig(t)
	r.zot.putDigest = dg("1")
	r.meter.blobs = map[string]int64{dg("1"): 6000}
	r.store.blobs[repoD1+"|"+dg("3")] = 5000 // really 5000 bytes
	w := r.do(tokA, "PUT", manPath(repoD1, "latest"), bytes.NewReader(snapshotManifest(map[string]int64{dg("3"): 1})), nil)
	if w.Code != 403 || !strings.Contains(w.Body.String(), CodeTenantQuota) {
		t.Fatalf("a manifest that says 1 byte for a 5000-byte blob: %d %s", w.Code, w.Body.String())
	}
	r.store.missing[dg("4")] = true
	w = r.do(tokA, "PUT", manPath(repoD1, "latest"), bytes.NewReader(snapshotManifest(map[string]int64{dg("4"): 1})), nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "MANIFEST_BLOB_UNKNOWN") {
		t.Fatalf("a blob the registry does not hold: %d %s", w.Code, w.Body.String())
	}
}

// 2.4.4: the tenant quota in registry bytes: refused at the start of an upload when already used up, and at the manifest when it would be passed;
// the device's own value beats the tenant's, which beats the platform's.
func TestTenantQuota(t *testing.T) {
	r := newLabRig(t)
	r.lab.Limits.MinInterval = 0
	r.zot.putDigest = dg("1")
	r.meter.blobs = map[string]int64{dg("1"): 4000, dg("2"): 5000} // 9000 of 10000 used by the other repositories
	put := func(layers map[string]int64) *httptest.ResponseRecorder {
		for d, n := range layers {
			r.store.blobs[repoD1+"|"+d] = n // the registry holds what the manifest says, here
		}
		return r.do(tokA, "PUT", manPath(repoD1, "latest"), bytes.NewReader(snapshotManifest(layers)), nil)
	}
	if w := put(map[string]int64{dg("3"): 900}); w.Code != 201 {
		t.Fatalf("9900 of 10000: %d %s", w.Code, w.Body.String())
	}
	w := put(map[string]int64{dg("3"): 1100})
	if w.Code != 403 || !strings.Contains(w.Body.String(), CodeTenantQuota) {
		t.Fatalf("10100 of 10000: %d %s", w.Code, w.Body.String())
	}
	// a blob the tenant already holds in another repository costs nothing again
	if w := put(map[string]int64{dg("1"): 4000, dg("3"): 900}); w.Code != 201 {
		t.Fatalf("a shared blob is counted once: %d", w.Code)
	}
	// the repository being replaced is not counted against itself
	if r.meter.asked[len(r.meter.asked)-1] != "acme|"+repoD1 {
		t.Fatalf("the meter was asked %v", r.meter.asked)
	}
	// the start of an upload: refused when the tenant is already at its quota
	r.meter.blobs = map[string]int64{dg("1"): 10000}
	w = r.do(tokA, "POST", upPath(repoD1), nil, nil)
	if w.Code != 403 || !strings.Contains(w.Body.String(), CodeTenantQuota) {
		t.Fatalf("at the quota: %d %s", w.Code, w.Body.String())
	}
	r.meter.blobs = map[string]int64{dg("1"): 9999}
	if w := r.do(tokA, "POST", upPath(repoD1), nil, nil); w.Code != 202 {
		t.Fatalf("just under: %d", w.Code)
	}
	// precedence
	r.sch.SetTenant(TenantInfo{Name: "acme", RegistryQuotaBytes: 20000})
	r.meter.blobs = map[string]int64{dg("1"): 15000}
	if w := r.do(tokA, "POST", upPath(repoD1), nil, nil); w.Code != 202 {
		t.Fatalf("the tenant's quota beats the platform's: %d", w.Code)
	}
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "dev1", Node: "node-a", StateEnabled: true, Tenant: "acme", TenantQuotaBytes: 12000})
	if w := r.do(tokA, "POST", upPath(repoD1), nil, nil); w.Code != 403 {
		t.Fatalf("the device's quota beats the tenant's: %d", w.Code)
	}
	// a meter that cannot answer is an outage, not an allowance
	r.meter.err = fmt.Errorf("zot down")
	r.meter.blobs = nil
	if w := r.do(tokA, "POST", upPath(repoD1), nil, nil); w.Code != 503 {
		t.Fatalf("the quota cannot be measured: %d", w.Code)
	}
	if w := put(map[string]int64{dg("3"): 1}); w.Code != 503 {
		t.Fatalf("the quota cannot be measured at the manifest: %d", w.Code)
	}
	// no quota at all
	r.meter.err = nil
	r.lab.Limits.TenantQuota = 0
	r.sch.SetTenant(TenantInfo{Name: "acme"})
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "dev1", Node: "node-a", StateEnabled: true, Tenant: "acme"})
	if w := put(map[string]int64{dg("3"): 1 << 40}); w.Code != 201 {
		t.Fatalf("no quota: %d", w.Code)
	}
}

// 2.4.4: below the reserve of free space a push is refused with 507, the exit snapshot included (the quota and the reserve always apply).
func TestRegistryReserve(t *testing.T) {
	r := newLabRig(t)
	r.free = 0.05
	r.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "dev1-0", UID: "pod-1", Node: "node-a", Phase: "Failed", Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}})
	for _, c := range []struct{ method, target string }{{"POST", upPath(repoD1)}, {"PUT", manPath(repoD1, "latest")}} {
		w := r.do(tokA, c.method, c.target, bytes.NewReader(snapshotManifest(nil)), nil)
		if w.Code != 507 || !strings.Contains(w.Body.String(), CodeRegistryFull) {
			t.Errorf("%s: %d %s", c.method, w.Code, w.Body.String())
		}
	}
	// reads are not affected
	if w := r.do(tokA, "GET", manPath(repoD1, "latest"), nil, nil); w.Code != 200 {
		t.Fatalf("a read: %d", w.Code)
	}
	r.free = 0.1
	if w := r.do(tokA, "POST", upPath(repoD1), nil, nil); w.Code != 202 {
		t.Fatalf("at the reserve exactly: %d", w.Code)
	}
}

// 2.4.4: after an accepted "latest" the previous manifest and the blobs only it used are deleted once the grace has passed.
func TestSupersededSnapshotIsDeletedAfterTheGrace(t *testing.T) {
	r := newLabRig(t)
	r.lab.Limits.MinInterval = 0
	oldM := snapshotManifest(map[string]int64{dg("a"): 10, dg("b"): 10})
	newM := snapshotManifest(map[string]int64{dg("b"): 10, dg("d"): 10})
	oldD, newD := sha256Digest(oldM), sha256Digest(newM)
	r.store.manifests[repoD1+"|latest"] = oldM
	r.zot.latest, r.zot.putDigest = oldD, newD
	r.store.manifests[repoD1+"|"+oldD] = oldM
	w := r.do(tokA, "PUT", manPath(repoD1, "latest"), bytes.NewReader(newM), nil)
	if w.Code != 201 {
		t.Fatalf("%d", w.Code)
	}
	// zot now holds the new one as latest
	r.store.manifests[repoD1+"|latest"] = newM
	r.store.manifests[repoD1+"|"+newD] = newM
	if r.sup.Pending() != 1 {
		t.Fatalf("the plan: %d", r.sup.Pending())
	}
	if n := r.sup.Sweep(context.Background()); n != 0 || len(r.store.deleted) != 0 {
		t.Fatal("nothing is deleted inside the grace")
	}
	*r.clk = r.clk.Add(2*time.Minute + time.Second)
	if n := r.sup.Sweep(context.Background()); n != 1 {
		t.Fatalf("swept %d", n)
	}
	got := strings.Join(r.store.deleted, "|")
	if !strings.Contains(got, "manifest "+repoD1+" "+oldD) || !strings.Contains(got, "blob "+repoD1+" "+dg("a")) {
		t.Fatalf("deleted: %s", got)
	}
	if strings.Contains(got, dg("b")) || strings.Contains(got, newD) {
		t.Fatalf("a blob the new snapshot uses must stay: %s", got)
	}
	// the first snapshot of a device has nothing to supersede
	r2 := newLabRig(t)
	r2.zot.putDigest = dg("9")
	r2.do(tokA, "PUT", manPath(repoD1, "latest"), bytes.NewReader(snapshotManifest(nil)), nil)
	if r2.sup.Pending() != 0 {
		t.Fatal("no previous snapshot, no plan")
	}
}

func TestSupersederKeepsWhatItCannotAccountFor(t *testing.T) {
	st := &memStore{manifests: map[string][]byte{}}
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := &Superseder{Store: st, Grace: time.Minute, Now: func() time.Time { return now }}
	oldM := snapshotManifest(map[string]int64{dg("a"): 1})
	st.manifests["r|"+sha256Digest(oldM)] = oldM
	// the replacement is not in the registry: nothing is deleted
	s.Plan("r", sha256Digest(oldM), dg("f"))
	now = now.Add(2 * time.Minute)
	s.Sweep(context.Background())
	if len(st.deleted) != 0 {
		t.Fatalf("deleted %v with no replacement present", st.deleted)
	}
	// the old one is already gone: nothing to do and no error
	s.Plan("r", dg("9"), dg("f"))
	now = now.Add(2 * time.Minute)
	s.Sweep(context.Background())
	if len(st.deleted) != 0 {
		t.Fatal(st.deleted)
	}
	// the same digest is no replacement
	s.Plan("r", dg("9"), dg("9"))
	if s.Pending() != 0 {
		t.Fatal("old == keep is no plan")
	}
}

// Past the age of the cluster view nothing a node asks for is answered: 503 with Retry-After.
func TestLabPathIsUnavailableOnAStaleView(t *testing.T) {
	r := newLabRig(t)
	*r.clk = r.clk.Add(16 * time.Minute)
	// the operator does not need the view; a node does (its pod cannot be placed)
	if w := r.do(tokA, "GET", manPath(repoD1, "latest"), nil, nil); w.Code != 503 || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("node: %d %v", w.Code, w.Header())
	}
	if w := r.do("operator-token", "GET", manPath(repoD1, "latest"), nil, nil); w.Code != 200 {
		t.Fatalf("operator: %d", w.Code)
	}
	r.sch.Confirm()
	if w := r.do(tokA, "GET", manPath(repoD1, "latest"), nil, nil); w.Code != 200 {
		t.Fatalf("after a contact: %d", w.Code)
	}
}

func TestParseLabPath(t *testing.T) {
	d := dg("a")
	for p, want := range map[string]labPath{
		"/v2/lab/ns/l/d/manifests/latest":  {"lab/ns/l/d", "ns", "l", "d", "manifests", "latest"},
		"/v2/lab/ns/l/d/blobs/" + d:        {"lab/ns/l/d", "ns", "l", "d", "blobs", d},
		"/v2/lab/ns/l/d/blobs/uploads/":    {"lab/ns/l/d", "ns", "l", "d", "uploads", ""},
		"/v2/lab/ns/l/d/blobs/uploads":     {"lab/ns/l/d", "ns", "l", "d", "uploads", ""},
		"/v2/lab/ns/l/d/blobs/uploads/s-1": {"lab/ns/l/d", "ns", "l", "d", "uploads", "s-1"},
	} {
		got, ok := parseLabPath(p)
		if !ok || got != want {
			t.Errorf("%s: %+v %v, want %+v", p, got, ok, want)
		}
	}
	for _, p := range []string{"/v2/lab/ns/l/d/tags/list", "/v2/lab/ns/l/d", "/v2/lab/ns/l", "/v2/lab/ns/l/d/referrers/" + d, "/v2/labx/ns/l/d/manifests/x", "/v2/base/manifests/x", "/v2/c/a/b/c/manifests/x"} {
		if _, ok := parseLabPath(p); ok {
			t.Errorf("%s must not parse", p)
		}
	}
}

func TestUploadID(t *testing.T) {
	for in, want := range map[string]string{
		"/v2/lab/a/b/c/blobs/uploads/abc-1?_state=x":          "abc-1",
		"http://localhost:5035/v2/lab/a/b/c/blobs/uploads/u9": "u9",
		"/v2/lab/a/b/c/blobs/uploads/":                        "",
		"/other":                                              "",
		"/v2/x/blobs/uploads/a/b":                             "",
	} {
		if got := uploadID(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// Review finding 7 / 2.4.6a: open upload sessions are limited per device (4) and per node (64); one node cannot use up another's room.
func TestUploadSessionsAreLimitedPerDeviceAndPerNode(t *testing.T) {
	r := newLabRig(t)
	r.lab.Limits.Budget = 0
	r.lab.Limits.UploadsPerNode = 6
	start := func(token, repo string) *httptest.ResponseRecorder {
		return r.do(token, "POST", upPath(repo), nil, nil)
	}
	for i := 0; i < 4; i++ {
		if w := start(tokA, repoD1); w.Code != 202 {
			t.Fatalf("session %d: %d", i, w.Code)
		}
	}
	w := start(tokA, repoD1)
	if w.Code != 429 || !strings.Contains(w.Body.String(), CodeUploadLimit) {
		t.Fatalf("the fifth for one device: %d %s", w.Code, w.Body.String())
	}
	// another device on the same node: the node's own limit (6 in all)
	r.sch.SetDevice(DeviceInfo{Namespace: "lg-acme", Lab: "lab1", Name: "dev3", Node: "node-a", StateEnabled: true, Tenant: "acme"})
	d3 := "lab/lg-acme/lab1/dev3"
	for i := 0; i < 2; i++ {
		if w := start(tokA, d3); w.Code != 202 {
			t.Fatalf("dev3 session %d: %d", i, w.Code)
		}
	}
	if w := start(tokA, d3); w.Code != 429 {
		t.Fatalf("the node is at its limit: %d", w.Code)
	}
	// another node is not affected
	if w := start(tokB, "lab/lg-acme/lab1/dev2"); w.Code != 202 {
		t.Fatalf("another node: %d", w.Code)
	}
	// finishing a session makes room; so does time (a session left alone for an hour is forgotten)
	loc := r.do(tokB, "POST", upPath("lab/lg-acme/lab1/dev2"), nil, nil).Header().Get("Location")
	_ = loc
	*r.clk = r.clk.Add(2 * time.Hour)
	r.sch.Confirm()
	if w := start(tokA, repoD1); w.Code != 202 {
		t.Fatalf("an hour later: %d", w.Code)
	}
}

// Review finding 10: when the old content is pushed again within the grace it is "latest" again and must not be deleted.
func TestSupersedeNeverDeletesTheCurrentLatest(t *testing.T) {
	st := &memStore{manifests: map[string][]byte{}, blobs: map[string]int64{}, missing: map[string]bool{}}
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := &Superseder{Store: st, Grace: time.Minute, Now: func() time.Time { return now }}
	a := snapshotManifest(map[string]int64{dg("a"): 1})
	b := snapshotManifest(map[string]int64{dg("b"): 1})
	st.manifests["r|"+sha256Digest(a)] = a
	st.manifests["r|"+sha256Digest(b)] = b
	st.manifests["r|latest"] = b
	s.Plan("r", sha256Digest(a), sha256Digest(b)) // a was replaced by b ...
	st.manifests["r|latest"] = a                  // ... and then a was pushed again
	now = now.Add(2 * time.Minute)
	s.Sweep(context.Background())
	if len(st.deleted) != 0 {
		t.Fatalf("the current latest was deleted: %v", st.deleted)
	}
}
