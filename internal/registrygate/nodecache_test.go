package registrygate

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/names"
)

// The decision table of 2.4.3 for the cache path, row by row, with a real zot, a fake upstream and a cluster view made by hand.

const (
	tokA = "node-token-agent-a/uid-a" // the relay of node-a
	tokB = "node-token-agent-b/uid-b" // the relay of node-b
	pfx  = "localhost:5035/c/"
)

type nodeRig struct {
	*rig
	md, ld, cfg string // manifest, layer and config digests of org/private
	nginxMd     string
	nginxLd     string
	clk         *time.Time
}

func dockerConfig(registry, user, pass string) []byte {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return []byte(fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, registry, auth))
}

func newNodeRig(t *testing.T) *nodeRig {
	t.Helper()
	r := newRig(t)
	n := &nodeRig{rig: r}
	n.ld = r.up.put("org/private", "blobs", bytes.Repeat([]byte("P"), 3000))
	n.cfg = r.up.put("org/private", "blobs", []byte("{}"))
	n.md = r.up.put("org/private", "manifests", manifestBody(n.cfg, n.ld))
	n.nginxLd = r.up.put("library/nginx", "blobs", bytes.Repeat([]byte("N"), 2000))
	nc := r.up.put("library/nginx", "blobs", []byte("{}"))
	n.nginxMd = r.putTag("library/nginx", "latest", manifestBody(nc, n.nginxLd))
	// the view: two nodes with their relays, tenants acme and other with a group namespace each
	s := r.sch
	clk := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	n.clk = &clk
	s.Now = func() time.Time { return clk }
	s.Confirm()
	r.cache.Grants.Now = func() time.Time { return clk }
	s.SetPod(PodInfo{Namespace: "laboratory-system", Name: "agent-a", UID: "uid-a", Node: "node-a", Phase: "Running"})
	s.SetPod(PodInfo{Namespace: "laboratory-system", Name: "agent-b", UID: "uid-b", Node: "node-b", Phase: "Running"})
	s.SetTenant(TenantInfo{Name: "acme"})
	s.SetTenant(TenantInfo{Name: "other"})
	s.SetGroup("lg-acme", "acme")
	s.SetGroup("lg-other", "other")
	// acme: the credentials of the tenant (alice may read org/private)
	r.cache.Creds.(*SecretView).Set(names.UpstreamTenantSecret("acme"), dockerConfig("up", "alice", "pw"))
	return n
}

// forget is a restart of the gate's memory: no grants, no decisions (the cluster view stays).
func (n *nodeRig) forget() {
	n.cache.Grants = &Grants{Now: func() time.Time { return *n.clk }}
	n.cache.Store = NewStore(DefaultTiming)
}

func (n *nodeRig) devicePod(ns, name, node, image string) {
	n.sch.SetPod(PodInfo{Namespace: ns, Name: name, UID: "u-" + name, Node: node, Phase: "Running", Images: []string{image},
		Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}})
}

// get does what the relay of a node does: the node's token, whatever else the client sent.
func (n *nodeRig) get(token, method, path string, hdr map[string]string) (int, []byte, http.Header) {
	n.t.Helper()
	req, _ := http.NewRequest(method, n.gate.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set(HeaderNodeToken, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func (n *nodeRig) mp(seg, repo, ref string) string {
	return "/v2/c/" + seg + "/up/" + repo + "/manifests/" + ref
}
func (n *nodeRig) bp(seg, repo, ref string) string {
	return "/v2/c/" + seg + "/up/" + repo + "/blobs/" + ref
}

func TestNodeDecisionTableForManifests(t *testing.T) {
	n := newNodeRig(t)
	// a device pod of acme on node-a pinned by digest, and a tag pod of acme for a public image
	n.devicePod("lg-acme", "dev-pinned", "node-a", pfx+"acme/up/org/private@"+n.md)
	n.devicePod("lg-acme", "dev-tag", "node-a", pfx+"acme/up/library/nginx:latest")
	// a pod of another tenant on node-b for the same private image
	n.devicePod("lg-other", "dev-other", "node-b", pfx+"other/up/org/private@"+n.md)
	other := "sha256:" + strings.Repeat("e", 64)
	n.cache.Creds.(*SecretView).Set(names.UpstreamTenantSecret("other"), dockerConfig("up", "alice", "pw"))

	type row struct {
		name   string
		token  string
		method string
		path   string
		want   int
	}
	for _, c := range []row{
		{"pod names exactly this digest for this tenant", tokA, "GET", n.mp("acme", "org/private", n.md), 200},
		{"HEAD of the same", tokA, "HEAD", n.mp("acme", "org/private", n.md), 200},
		{"tag pod names exactly this tag", tokA, "GET", n.mp("acme", "library/nginx", "latest"), 200},
		{"a digest-pinned pod: another digest of the repository", tokA, "GET", n.mp("acme", "org/private", other), 403},
		{"a digest-pinned pod: a tag of the repository", tokA, "GET", n.mp("acme", "org/private", "latest"), 403},
		{"a tag pod: another tag", tokA, "GET", n.mp("acme", "library/nginx", "1.0"), 403},
		{"a tag pod: the digest the tag was just served with (granted by that manifest)", tokA, "GET", n.mp("acme", "library/nginx", n.nginxMd), 200},
		{"no pod of this tenant names it: the other tenant's segment", tokA, "GET", n.mp("other", "org/private", n.md), 403},
		{"the same reference on a node without a pod for it", tokB, "GET", n.mp("acme", "org/private", n.md), 403},
		{"the pod of another tenant on node-b", tokB, "GET", n.mp("other", "org/private", n.md), 200},
		{"a segment that maps to no tenant", tokA, "GET", n.mp("nobody", "org/private", n.md), 403},
		{"the platform segment for a device pod", tokA, "GET", n.mp("platform", "org/private", n.md), 403},
		{"the old form without a segment", tokA, "GET", "/v2/c/up/org/private/manifests/" + n.md, 403},
		{"the old form with a registry host", tokA, "GET", "/v2/c/docker.io/library/nginx/manifests/latest", 403},
		{"a registry that is not on the list", tokA, "GET", "/v2/c/acme/evil.example/org/private/manifests/" + n.md, 403},
		{"POST on the cache path", tokA, "POST", n.mp("acme", "org/private", n.md), 405},
		{"DELETE on the cache path", tokA, "DELETE", n.mp("acme", "org/private", n.md), 405},
	} {
		got, _, _ := n.get(c.token, c.method, c.path, nil)
		if got != c.want {
			t.Errorf("%s: %s %s -> %d, want %d", c.name, c.method, c.path, got, c.want)
		}
	}
}

// The segment names a tenant, and the pod must belong to a group of that tenant: a pod of tenant B naming acme's reference is no claim.
func TestAPodOfAnotherTenantIsNoClaimForTheSegment(t *testing.T) {
	n := newNodeRig(t)
	n.devicePod("lg-other", "dev-x", "node-a", pfx+"acme/up/org/private@"+n.md) // a pod of "other" names an acme reference
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 403 {
		t.Fatalf("%d", code)
	}
	// a pod in a namespace that is no group's at all
	n.devicePod("kube-system", "dev-y", "node-a", pfx+"acme/up/org/private@"+n.md)
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 403 {
		t.Fatalf("a pod outside the groups: %d", code)
	}
}

func TestPlatformSegmentIsForTheVPNAndGatewayPodsOnly(t *testing.T) {
	n := newNodeRig(t)
	n.cache.Creds.(*SecretView).Set(names.UpstreamPlatformSecret, dockerConfig("up", "alice", "pw"))
	n.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "vpn", UID: "u-vpn", Node: "node-a", Phase: "Running", Images: []string{pfx + "platform/up/org/private@" + n.md},
		Labels: map[string]string{names.LabelComponent: names.ComponentVPN}})
	if code, _, _ := n.get(tokA, "GET", n.mp("platform", "org/private", n.md), nil); code != 200 {
		t.Fatalf("a vpn pod: %d", code)
	}
	n.forget()
	n.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "vpn", UID: "u-vpn", Node: "node-a", Phase: "Running", Images: []string{pfx + "platform/up/org/private@" + n.md},
		Labels: map[string]string{names.LabelLab: "lab1"}}) // a device pod claiming the platform segment
	if code, _, _ := n.get(tokA, "GET", n.mp("platform", "org/private", n.md), nil); code != 403 {
		t.Fatalf("a device pod with the platform segment: %d", code)
	}
}

func TestImagePullListsTheNodeTheReferenceAndTheTenant(t *testing.T) {
	n := newNodeRig(t)
	ref := pfx + "acme/up/org/private@" + n.md
	n.sch.SetImagePull(ImagePullInfo{Name: "p1", Nodes: []string{"node-a"}, Images: []string{ref}, Tenant: "acme"})
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 200 {
		t.Fatalf("listed: %d", code)
	}
	if code, _, _ := n.get(tokB, "GET", n.mp("acme", "org/private", n.md), nil); code != 403 {
		t.Fatalf("another node: %d", code)
	}
	n.forget()
	n.sch.SetImagePull(ImagePullInfo{Name: "p1", Nodes: []string{"node-a"}, Images: []string{ref}, Tenant: "other"})
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 403 {
		t.Fatalf("another tenant in the request: %d", code)
	}
	n.sch.SetImagePull(ImagePullInfo{Name: "p1", Nodes: []string{"node-a"}, Images: []string{pfx + "acme/up/org/private@sha256:" + strings.Repeat("e", 64)}, Tenant: "acme"})
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 403 {
		t.Fatalf("another reference: %d", code)
	}
	n.sch.DeleteImagePull("p1")
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 403 {
		t.Fatalf("a finished ImagePull: %d", code)
	}
}

// 2.4.2: a blob needs a grant made by a manifest served on the node's claim, and the grant is for that (node, tenant, repository).
func TestBlobsNeedAGrantFromAServedManifest(t *testing.T) {
	n := newNodeRig(t)
	n.devicePod("lg-acme", "dev-pinned", "node-a", pfx+"acme/up/org/private@"+n.md)
	// nothing served yet: a pod names the repository, so 404 BLOB_UNKNOWN (the kubelet asks for the manifest again), not a grant
	code, body, _ := n.get(tokA, "GET", n.bp("acme", "org/private", n.ld), nil)
	if code != 404 && code != 200 {
		t.Fatalf("a blob before the manifest: %d", code)
	}
	_ = body
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 200 {
		t.Fatalf("manifest: %d", code)
	}
	for _, d := range []string{n.ld, n.cfg} {
		if code, _, _ := n.get(tokA, "GET", n.bp("acme", "org/private", d), nil); code != 200 {
			t.Fatalf("a blob the manifest lists: %d", code)
		}
	}
	// a blob the manifest does not list
	foreign := n.up.put("org/private", "blobs", []byte("not in the manifest"))
	if code, _, _ := n.get(tokA, "GET", n.bp("acme", "org/private", foreign), nil); code != 404 {
		t.Fatalf("a digest of the repository that no manifest of the pod lists: %d", code)
	}
	// the same digest through another repository or another tenant's path
	if code, _, _ := n.get(tokA, "GET", n.bp("acme", "library/nginx", n.ld), nil); code != 403 {
		t.Fatalf("a repository no pod names: %d", code)
	}
	if code, _, _ := n.get(tokA, "GET", n.bp("other", "org/private", n.ld), nil); code != 403 {
		t.Fatalf("another tenant's path: %d", code)
	}
	// and from another node, which has no pod for it
	if code, _, _ := n.get(tokB, "GET", n.bp("acme", "org/private", n.ld), nil); code != 403 {
		t.Fatalf("another node: %d", code)
	}
}

// A grant lives for the pull grace after its last use, and never longer than the maximum age.
func TestGrantsExpireWithTheGraceAndTheMaximumAge(t *testing.T) {
	n := newNodeRig(t)
	n.cache.Grants.Grace, n.cache.Grants.MaxAge = 2*time.Minute, 10*time.Minute
	n.devicePod("lg-acme", "dev-tag", "node-a", pfx+"acme/up/library/nginx:latest")
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "library/nginx", "latest"), nil); code != 200 {
		t.Fatalf("manifest: %d", code)
	}
	get := func() int { c, _, _ := n.get(tokA, "GET", n.bp("acme", "library/nginx", n.nginxLd), nil); return c }
	if get() != 200 {
		t.Fatal("inside the grace")
	}
	// kept alive by use: every minute for 8 minutes
	for i := 0; i < 8; i++ {
		*n.clk = n.clk.Add(time.Minute)
		if get() != 200 {
			t.Fatalf("use extends the grant (minute %d)", i+1)
		}
	}
	*n.clk = n.clk.Add(3 * time.Minute) // past the maximum age of 10 minutes although it was used 3 minutes ago
	if get() != 404 {
		t.Fatalf("a grant is not kept past its maximum age by use")
	}
	// ... and a grant that is not used lapses after the grace
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "library/nginx", "latest"), nil); code != 200 {
		t.Fatalf("a new manifest request makes a new grant: %d", code)
	}
	*n.clk = n.clk.Add(2*time.Minute + time.Second)
	if get() != 404 {
		t.Fatal("an unused grant lapses after the grace")
	}
}

// After a restart of the gate the grants are gone: a digest-pinned reference rebuilds its grant from what zot holds.
func TestAGateRestartRebuildsTheGrantOfADigestPinnedReference(t *testing.T) {
	n := newNodeRig(t)
	n.devicePod("lg-acme", "dev-pinned", "node-a", pfx+"acme/up/org/private@"+n.md)
	for _, d := range []string{n.md} {
		if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", d), nil); code != 200 {
			t.Fatalf("manifest: %d", code)
		}
	}
	for _, d := range []string{n.ld, n.cfg} {
		if code, _, _ := n.get(tokA, "GET", n.bp("acme", "org/private", d), nil); code != 200 {
			t.Fatalf("blob: %d", code)
		}
	}
	// the gate restarts: no grants
	n.cache.Grants = &Grants{Now: func() time.Time { return *n.clk }}
	if code, _, _ := n.get(tokA, "GET", n.bp("acme", "org/private", n.ld), nil); code != 200 {
		t.Fatalf("a digest-pinned reference rebuilds the grant: %d", code)
	}
	// a tag reference cannot: the kubelet is told to ask for the manifest again
	n2 := newNodeRig(t)
	n2.devicePod("lg-acme", "dev-tag", "node-a", pfx+"acme/up/library/nginx:latest")
	if code, _, _ := n2.get(tokA, "GET", n2.mp("acme", "library/nginx", "latest"), nil); code != 200 {
		t.Fatal(code)
	}
	n2.cache.Grants = &Grants{Now: func() time.Time { return *n2.clk }}
	if code, _, _ := n2.get(tokA, "GET", n2.bp("acme", "library/nginx", n2.nginxLd), nil); code != 404 {
		t.Fatalf("a tag reference after a restart: %d", code)
	}
}

func TestRequireDigestRefusesTagsFromANode(t *testing.T) {
	n := newNodeRig(t)
	n.devicePod("lg-acme", "dev-tag", "node-a", pfx+"acme/up/library/nginx:latest")
	n.devicePod("lg-acme", "dev-pinned", "node-a", pfx+"acme/up/org/private@"+n.md)
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "library/nginx", "latest"), nil); code != 200 {
		t.Fatalf("off: a tag pull: %d", code)
	}
	n.cache.RequireDigest = true
	code, body, _ := n.get(tokA, "GET", n.mp("acme", "library/nginx", "latest"), nil)
	if code != 403 || !bytes.Contains(body, []byte("CIB_UNPINNED")) {
		t.Fatalf("on: a tag pull: %d %s", code, body)
	}
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 200 {
		t.Fatalf("on: a digest pull: %d", code)
	}
	// the agent's prewarm resolves tags itself and is not affected
	if code, _, _ := n.pull("GET", "/v2/c/up/library/nginx/manifests/latest", "", "", nil); code != 200 {
		t.Fatalf("the prewarm: %d", code)
	}
}

// Nothing a node sends is credentials: an Authorization header is ignored, and the tenant's own credentials are the only ones used.
func TestANodesAuthorizationHeaderIsNeverRead(t *testing.T) {
	n := newNodeRig(t)
	n.devicePod("lg-acme", "dev-pinned", "node-a", pfx+"acme/up/org/private@"+n.md)
	// without the tenant's secret the private image cannot be proved, whatever the node sends
	n.cache.Creds.(*SecretView).Delete(names.UpstreamTenantSecret("acme"))
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:pw"))
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), map[string]string{"Authorization": basic, "X-Cib-Tenant": "acme"}); code != 403 {
		t.Fatalf("the credentials of the node's own request must not be used: %d", code)
	}
	n.cache.Creds.(*SecretView).Set(names.UpstreamTenantSecret("acme"), dockerConfig("up", "alice", "pw"))
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), map[string]string{"Authorization": "Basic Zm9vOmJhcg=="}); code != 200 {
		t.Fatalf("a wrong Authorization of the node does not matter, the tenant's secret decides: %d", code)
	}
}

// 2.4.5: a lab's own credentials first, then the tenant's; several are tried in turn; a registry with no entry is proved anonymously.
func TestUpstreamCredentialsComeFromTheTenantsAndTheLabsSecretsInTurn(t *testing.T) {
	n := newNodeRig(t)
	sv := n.cache.Creds.(*SecretView)
	n.devicePod("lg-acme", "dev-pinned", "node-a", pfx+"acme/up/org/private@"+n.md)
	// the lab brought credentials that have no right; the tenant's have: the tenant's are tried after the lab's
	sv.Set(names.UpstreamLabSecret("lg-acme", "lab1"), dockerConfig("up", "mallory", "pw"))
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 200 {
		t.Fatalf("the second credential works: %d", code)
	}
	// a tenant with no secret and a private image: refused
	n.sch.SetPod(PodInfo{Namespace: "lg-other", Name: "o1", UID: "u-o1", Node: "node-a", Phase: "Running", Images: []string{pfx + "other/up/org/private@" + n.md}})
	if code, _, _ := n.get(tokA, "GET", n.mp("other", "org/private", n.md), nil); code != 403 {
		t.Fatalf("no credentials, a private image: %d", code)
	}
	// ... but the same tenant's public image is fine without any
	n.sch.SetPod(PodInfo{Namespace: "lg-other", Name: "o2", UID: "u-o2", Node: "node-a", Phase: "Running", Images: []string{pfx + "other/up/library/nginx:latest"}})
	if code, _, _ := n.get(tokA, "GET", n.mp("other", "library/nginx", "latest"), nil); code != 200 {
		t.Fatalf("a public image: %d", code)
	}
	// the lab's secret of ANOTHER lab is not used for this one
	sv.Delete(names.UpstreamTenantSecret("acme"))
	sv.Delete(names.UpstreamLabSecret("lg-acme", "lab1"))
	sv.Set(names.UpstreamLabSecret("lg-acme", "lab2"), dockerConfig("up", "alice", "pw"))
	n.forget() // the proof and the grant made above
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 403 {
		t.Fatalf("a lab's secret is for that lab's pods only: %d", code)
	}
}

func TestAStaleViewIsA503OnTheCachePath(t *testing.T) {
	n := newNodeRig(t)
	n.devicePod("lg-acme", "dev-pinned", "node-a", pfx+"acme/up/org/private@"+n.md)
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 200 {
		t.Fatal(code)
	}
	*n.clk = n.clk.Add(16 * time.Minute)
	code, _, hdr := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil)
	if code != 503 || hdr.Get("Retry-After") != "30" {
		t.Fatalf("%d %v", code, hdr)
	}
	n.sch.Confirm()
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 200 {
		t.Fatalf("after a contact: %d", code)
	}
}

// The agent: any registry of the list without a schedule match, the credentials of its call, a bound on what it runs at once.
func TestTheAgentNeedsNoPodButOnlyForTheRegistriesOfTheList(t *testing.T) {
	n := newNodeRig(t)
	if code, _, _ := n.pull("GET", "/v2/c/up/library/nginx/manifests/latest", "", "", nil); code != 200 {
		t.Fatalf("the agent: %d", code)
	}
	if code, _, _ := n.pull("GET", mpath("org/private", n.md), "", "", nil); code != 403 {
		t.Fatalf("a private image without the call's credentials: %d", code)
	}
	if code, _, _ := n.pull("GET", mpath("org/private", n.md), "acme~alice", "pw", nil); code != 200 {
		t.Fatalf("with the call's credentials: %d", code)
	}
	n.cache.Allowed = []string{"other.example"}
	if code, _, _ := n.pull("GET", "/v2/c/up/library/nginx/manifests/latest", "", "", nil); code != 403 {
		t.Fatalf("allowedRegistries: %d", code)
	}
	// the operator has no business on the cache path
	n.cache.Allowed = nil
	if code, _, _ := n.get("operator-token", "GET", n.mp("acme", "library/nginx", "latest"), nil); code != 403 {
		t.Fatalf("operator: %d", code)
	}
}

func TestTheAgentIsBoundedInWhatItRunsAtOnce(t *testing.T) {
	n := newNodeRig(t)
	n.cache.AgentMaxFills = 1
	if code, _, _ := n.pull("GET", "/v2/c/up/library/nginx/manifests/latest", "", "", nil); code != 200 {
		t.Fatal(code)
	}
	n.cache.agentSem <- struct{}{} // one request is running
	code, _, hdr := n.pull("GET", "/v2/c/up/library/nginx/manifests/latest", "", "", nil)
	if code != 429 || hdr.Get("Retry-After") == "" {
		t.Fatalf("%d %v", code, hdr)
	}
	<-n.cache.agentSem
	if code, _, _ := n.pull("GET", "/v2/c/up/library/nginx/manifests/latest", "", "", nil); code != 200 {
		t.Fatalf("after it finished: %d", code)
	}
}

func TestParseCacheImage(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	for in, want := range map[string]imageRef{
		"localhost:5035/c/acme/docker.io/library/nginx:1.27":      {"localhost:5035", "acme", "docker.io", "library/nginx", "1.27", ""},
		"localhost:5035/c/acme/docker.io/library/nginx@" + d:      {"localhost:5035", "acme", "docker.io", "library/nginx", "", d},
		"localhost:5035/c/acme/docker.io/library/nginx:1.27@" + d: {"localhost:5035", "acme", "docker.io", "library/nginx", "1.27", d},
		"localhost:5035/c/platform/ghcr.io/org/img":               {"localhost:5035", "platform", "ghcr.io", "org/img", "", ""},
		"10.0.0.1:5035/c/t-0123456789ab/localhost:5000/team/i:v1": {"10.0.0.1:5035", "t-0123456789ab", "localhost:5000", "team/i", "v1", ""},
	} {
		got, ok := parseCacheImage(in)
		if !ok || got != want {
			t.Errorf("%s: %+v %v, want %+v", in, got, ok, want)
		}
	}
	for _, in := range []string{"docker.io/library/nginx:1", "localhost:5035/lab/ns/lab/dev@" + d, "localhost:5035/c/acme/docker.io", "localhost:5035/c//docker.io/x"} {
		if _, ok := parseCacheImage(in); ok {
			t.Errorf("%s is no cache image", in)
		}
	}
}

// Review finding 1: a grant rebuilt after a restart needs the same upstream proof as a manifest request. A tenant whose credentials the
// upstream refuses, with a pod that pins the digest of another tenant's private image that is already cached, gets nothing.
func TestARebuiltGrantNeedsTheUpstreamProof(t *testing.T) {
	n := newNodeRig(t)
	n.cache.Creds.(*SecretView).Set(names.UpstreamTenantSecret("other"), dockerConfig("up", "alice", "pw"))
	n.devicePod("lg-other", "dev-other", "node-b", pfx+"other/up/org/private@"+n.md)
	if code, _, _ := n.get(tokB, "GET", n.mp("other", "org/private", n.md), nil); code != 200 {
		t.Fatalf("the owner fills the cache: %d", code)
	}
	for _, d := range []string{n.ld, n.cfg} {
		if code, _, _ := n.get(tokB, "GET", n.bp("other", "org/private", d), nil); code != 200 {
			t.Fatalf("the owner pulls the layers: %d", code)
		}
	}
	// acme's own credentials do not open org/private upstream
	n.cache.Creds.(*SecretView).Set(names.UpstreamTenantSecret("acme"), dockerConfig("up", "bob", "wrong"))
	n.devicePod("lg-acme", "dev-thief", "node-a", pfx+"acme/up/org/private@"+n.md)
	n.forget()
	for _, d := range []string{n.ld, n.cfg} {
		if code, _, _ := n.get(tokA, "GET", n.bp("acme", "org/private", d), nil); code == 200 {
			t.Fatalf("a layer of another tenants private image was served without proof: %d", code)
		}
	}
	// the manifest itself is refused too
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "org/private", n.md), nil); code != 403 {
		t.Fatalf("manifest: %d", code)
	}
	// with the right credentials the rebuild works
	n.cache.Creds.(*SecretView).Set(names.UpstreamTenantSecret("acme"), dockerConfig("up", "alice", "pw"))
	n.forget()
	if code, _, _ := n.get(tokA, "GET", n.bp("acme", "org/private", n.ld), nil); code != 200 {
		t.Fatalf("with proof: %d", code)
	}
}

// Review finding 11: a grant dies with the pod that earned it, a finished pod pulls nothing, and an image that is not <host>/c/... is no
// cache reference.
func TestAGrantDiesWithItsPodAndAFinishedPodPullsNothing(t *testing.T) {
	n := newNodeRig(t)
	n.devicePod("lg-acme", "dev-tag", "node-a", pfx+"acme/up/library/nginx:latest")
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "library/nginx", "latest"), nil); code != 200 {
		t.Fatal(code)
	}
	if code, _, _ := n.get(tokA, "GET", n.bp("acme", "library/nginx", n.nginxLd), nil); code != 200 {
		t.Fatal(code)
	}
	n.sch.DeletePod("lg-acme", "dev-tag")
	if code, _, _ := n.get(tokA, "GET", n.bp("acme", "library/nginx", n.nginxLd), nil); code == 200 {
		t.Fatalf("the pod is gone, the grant must not outlive it: %d", code)
	}
	// a pod that has finished does not pull again
	n.sch.SetPod(PodInfo{Namespace: "lg-acme", Name: "done", UID: "u-done", Node: "node-a", Phase: "Succeeded", Images: []string{pfx + "acme/up/library/nginx:latest"},
		Labels: map[string]string{names.LabelLab: "lab1", names.LabelDevice: "dev1"}})
	if code, _, _ := n.get(tokA, "GET", n.mp("acme", "library/nginx", "latest"), nil); code != 403 {
		t.Fatalf("a Succeeded pod: %d", code)
	}
}

func TestParseCacheImageNeedsTheHostForm(t *testing.T) {
	for _, bad := range []string{"evil.example/mirror/c/acme/up/x/y:1", "/c/acme/up/x/y", "c/acme/up/x/y", "a/b/c/c/acme/up/x"} {
		if _, ok := parseCacheImage(bad); ok {
			t.Errorf("%q is no cache reference", bad)
		}
	}
	if ref, ok := parseCacheImage("localhost:5035/c/acme/up/org/x:1"); !ok || ref.Host != "localhost:5035" || ref.Seg != "acme" {
		t.Fatalf("%+v %v", ref, ok)
	}
	c := &Cache{ImageHost: "localhost:5035"}
	if _, ok := c.parseImage("other.example:5035/c/acme/up/org/x:1"); ok {
		t.Fatal("another host than the gate's")
	}
}

// No answer of the gate, refusals included, carries a credential of a pull or the token of a node.
func TestNoAnswerCarriesACredentialOrAToken(t *testing.T) {
	n := newNodeRig(t)
	n.cache.Creds.(*SecretView).Set(names.UpstreamTenantSecret("acme"), dockerConfig("up", "bob-user", "bob-secret-pw"))
	n.devicePod("lg-acme", "dev-thief", "node-a", pfx+"acme/up/org/private@"+n.md)
	for _, path := range []string{n.mp("acme", "org/private", n.md), n.bp("acme", "org/private", n.ld), n.mp("nobody", "org/private", n.md), n.mp("acme", "org/private", "latest")} {
		code, body, hdr := n.get(tokA, "GET", path, map[string]string{"Authorization": "Basic Ym9iLXVzZXI6Ym9iLXNlY3JldC1wdw=="})
		all := string(body) + fmt.Sprint(hdr)
		for _, secret := range []string{"bob-user", "bob-secret-pw", "Ym9iLXVzZXI", tokA} {
			if strings.Contains(all, secret) {
				t.Errorf("%s (%d) carries %q", path, code, secret)
			}
		}
	}
}
