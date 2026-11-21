package registrygate

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// corrupt makes the upstream answer a blob with other bytes than its digest names (same length: only the content is wrong).
func (r *rig) corrupt(repo, digest string, with []byte) {
	r.t.Helper()
	r.up.mu.Lock()
	defer r.up.mu.Unlock()
	r.up.objects["/v2/"+repo+"/blobs/"+digest] = with
}

// pullBody is a GET that tolerates a body cut short: it returns what arrived and whether the transfer completed.
func (r *rig) pullBody(path, user, pass string) (status int, body []byte, complete bool) {
	r.t.Helper()
	seg, up := "t0", ""
	if user != "" {
		seg, up, _ = splitTenantUser(user)
	}
	path = "/v2/c/" + seg + path[len("/v2/c"):]
	req, _ := http.NewRequest(http.MethodGet, r.gate.URL+path, nil)
	req.Header.Set(HeaderNodeToken, "agent-token")
	if up != "" {
		req.SetBasicAuth(up, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, false
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err == nil
}

func splitTenantUser(s string) (seg, user string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '~' {
			return s[:i], s[i+1:], true
		}
	}
	return "t0", s, false
}

// The upstream sends bytes that do not hash to the digest asked for: the gate must not store them and must not let the client finish
// the download (a short body, never the whole of the wrong content), whoever else checks.
func TestFillRefusesABlobThatDoesNotMatchItsDigest(t *testing.T) {
	r := newRig(t)
	good := bytes.Repeat([]byte("G"), 4096)
	d := r.up.put("org/private", "blobs", good)
	bad := bytes.Repeat([]byte("B"), len(good))
	r.corrupt("org/private", d, bad)

	_, body, complete := r.pullBody(bpath("org/private", d), alice, "pw")
	if complete && bytes.Equal(body, bad) {
		t.Fatal("the client was served a whole blob that does not match its digest")
	}
	if len(body) >= len(bad) {
		t.Fatalf("the last byte of an unverified blob must be held back: %d of %d bytes arrived", len(body), len(bad))
	}
	if r.inZot("c/up/org/private", "blobs", d) {
		t.Fatal("a blob that does not match its digest was stored")
	}
	// the right bytes afterwards fill normally (nothing of the refused fill is left behind)
	r.corrupt("org/private", d, good)
	if code, body, _ := r.pull("GET", bpath("org/private", d), alice, "pw", nil); code != 200 || !bytes.Equal(body, good) {
		t.Fatalf("after the upstream is right again: %d", code)
	}
}

// The same when the miss is streamed through unstored (disk above its mark).
func TestStreamThroughRefusesABlobThatDoesNotMatchItsDigest(t *testing.T) {
	r := newRig(t)
	r.cache.DiskFull = func() bool { return true }
	good := bytes.Repeat([]byte("G"), 4096)
	d := r.up.put("org/private", "blobs", good)
	bad := bytes.Repeat([]byte("B"), len(good))
	r.corrupt("org/private", d, bad)

	_, body, complete := r.pullBody(bpath("org/private", d), alice, "pw")
	if complete || len(body) >= len(bad) {
		t.Fatalf("the client was served a blob that does not match its digest: %d bytes, complete %v", len(body), complete)
	}
	r.corrupt("org/private", d, good)
	if code, body, _ := r.pull("GET", bpath("org/private", d), alice, "pw", nil); code != 200 || !bytes.Equal(body, good) {
		t.Fatalf("a right blob streams through: %d", code)
	}
}

// A resumed download is how a client gets round a cut: the first transfer stops one byte short, the next asks for the rest. A range is not
// stored and cannot be hashed, so a range of a blob that was refused must not be passed on from the upstream at all.
func TestRangeOfAnUnverifiedBlobIsNotPassedOn(t *testing.T) {
	for _, full := range []bool{false, true} {
		r := newRig(t)
		r.cache.DiskFull = func() bool { return full }
		good := bytes.Repeat([]byte("G"), 4096)
		d := r.up.put("org/private", "blobs", good)
		bad := bytes.Repeat([]byte("B"), len(good))
		r.corrupt("org/private", d, bad)

		code, body, _ := r.pull("GET", bpath("org/private", d), alice, "pw", map[string]string{"Range": "bytes=4095-"})
		if code == http.StatusPartialContent || bytes.Equal(body, bad[4095:]) {
			t.Fatalf("diskFull=%v: the last byte of a blob that does not match its digest was served (%d, %d bytes)", full, code, len(body))
		}
		if r.inZot("c/up/org/private", "blobs", d) {
			t.Fatalf("diskFull=%v: a blob that does not match its digest was stored", full)
		}
	}
}

// What an upstream says when it refuses or fails is never passed on to the client of the gate (kubelet): the answers carry the gate's own
// words only.
func TestGateDoesNotEchoTheBodyOfAnUpstreamError(t *testing.T) {
	const secret = "INTERNAL-SECRET token=abc123"
	r := newRig(t)
	layer := bytes.Repeat([]byte("L"), 2048)
	ld := r.up.put("org/private", "blobs", layer)
	md := r.up.put("org/private", "manifests", manifestBody(r.up.put("org/private", "blobs", []byte("{}")), ld))

	r.up.mu.Lock()
	r.up.failBody = secret
	r.up.mu.Unlock()
	for _, code := range []int{401, 403, 404, 429, 500, 503} {
		r.up.setFail(code)
		for _, p := range []string{mpath("org/private", md), bpath("org/private", ld)} {
			if st, body, _ := r.pull("GET", p, alice, "pw", nil); strings.Contains(string(body), "SECRET") || strings.Contains(string(body), "abc123") {
				t.Errorf("%d %s: the upstream's text reached the client (status %d): %s", code, p, st, body)
			}
		}
	}
	r.up.setFail(0)

	// the CDN a blob is redirected to fails with a body
	r.up.mu.Lock()
	r.up.redirect, r.up.cdnStatus = true, 500
	r.up.mu.Unlock()
	if st, body, _ := r.pull("GET", bpath("org/private", ld), alice, "pw", nil); st == 200 || strings.Contains(string(body), "SECRET") {
		t.Errorf("a failing CDN: %d %s", st, body)
	}
}
