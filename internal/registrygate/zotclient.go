package registrygate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Zot is the gate's own client of zot: the account `gate`, which may read and write the cache repositories (c/**) and nothing else.
type Zot struct {
	Base *url.URL // http://127.0.0.1:5001
	User string
	Pass string
	HTTP *http.Client
}

// NewZot is a client for the zot at addr (host:port, plain HTTP, the loopback of the pod).
func NewZot(addr, user, pass string) *Zot {
	return &Zot{Base: &url.URL{Scheme: schemeHTTP, Host: addr}, User: user, Pass: pass, HTTP: &http.Client{}}
}

func (z *Zot) req(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, z.Base.JoinPath(path).String(), body)
	if err != nil {
		return nil, err
	}
	r.SetBasicAuth(z.User, z.Pass)
	return r, nil
}

func objPath(repo, kind, ref string) string { return "/v2/" + repo + "/" + kind + "/" + ref }

// Head is zot's answer for an object of a cache repository: its headers, and whether it is there.
func (z *Zot) Head(ctx context.Context, repo, kind, ref string) (http.Header, bool, error) {
	r, err := z.req(ctx, http.MethodHead, objPath(repo, kind, ref), nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := z.HTTP.Do(r)
	if err != nil {
		return nil, false, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Header, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("zot answered %d to HEAD %s", resp.StatusCode, objPath(repo, kind, ref))
}

// Get opens an object of a cache repository; rangeHdr is the client's Range header, if any. The caller closes the body. A missing object
// is (nil, nil).
func (z *Zot) Get(ctx context.Context, repo, kind, ref, rangeHdr, accept string) (*http.Response, error) {
	r, err := z.req(ctx, http.MethodGet, objPath(repo, kind, ref), nil)
	if err != nil {
		return nil, err
	}
	if rangeHdr != "" {
		r.Header.Set("Range", rangeHdr)
	}
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	resp, err := z.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		return resp, nil
	case http.StatusNotFound:
		drain(resp)
		return nil, nil
	}
	drain(resp)
	return nil, fmt.Errorf("zot answered %d to GET %s", resp.StatusCode, objPath(repo, kind, ref))
}

// PutManifest stores a manifest under a reference (a digest, or a tag).
func (z *Zot) PutManifest(ctx context.Context, repo, ref, contentType string, body []byte) error {
	r, err := z.req(ctx, http.MethodPut, objPath(repo, kindManifests, ref), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", contentType)
	resp, err := z.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("zot answered %d to PUT manifest %s", resp.StatusCode, ref)
	}
	return nil
}

// PutBlob stores a blob of a known size and digest (a monolithic upload: zot checks the digest at the commit, so a stream that is
// cut or altered is refused and nothing is kept).
func (z *Zot) PutBlob(ctx context.Context, repo, digest string, body io.Reader, size int64) error {
	start, err := z.req(ctx, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil)
	if err != nil {
		return err
	}
	resp, err := z.HTTP.Do(start)
	if err != nil {
		return err
	}
	loc := resp.Header.Get("Location")
	status := resp.StatusCode
	drain(resp)
	if status != http.StatusAccepted || loc == "" {
		return fmt.Errorf("zot answered %d to the start of an upload", status)
	}
	lu, err := z.Base.Parse(loc)
	if err != nil {
		return err
	}
	// the upload URL is zot's own: only its path and query are used, never a host a response names
	q := lu.Query()
	q.Set("digest", digest)
	target := *z.Base
	target.Path, target.RawQuery = lu.Path, q.Encode()
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), body)
	if err != nil {
		return err
	}
	put.SetBasicAuth(z.User, z.Pass)
	put.ContentLength = size
	put.Header.Set("Content-Type", "application/octet-stream")
	resp, err = z.HTTP.Do(put)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("zot answered %d to the commit of blob %s", resp.StatusCode, digest)
	}
	return nil
}

// ErrNotMounted: zot did not take the blob from the other repository (it is not there, or the mount is refused).
var ErrNotMounted = errors.New("blob not mounted")

// Mount links a blob of repository from into repo (no data moves; zot checks create on repo and read on from, and the gate account has
// both on the whole cache tree).
func (z *Zot) Mount(ctx context.Context, repo, digest, from string) error {
	r, err := z.req(ctx, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil)
	if err != nil {
		return err
	}
	q := url.Values{"mount": {digest}, "from": {from}}
	r.URL.RawQuery = q.Encode()
	resp, err := z.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusCreated {
		return nil
	}
	if resp.StatusCode == http.StatusAccepted {
		return ErrNotMounted // zot opened a normal upload instead; it is left to expire
	}
	return fmt.Errorf("zot answered %d to a mount", resp.StatusCode)
}
