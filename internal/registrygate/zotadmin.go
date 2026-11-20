package registrygate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// The calls the gate makes to zot with its own account for what it must do itself on the snapshot repositories (lab/**): read a manifest
// to account for it, and delete what a newer snapshot has replaced. No node can delete anything; only this code does.

// ManifestBody returns the manifest at ref of a repository, or nil when it is not there. At most maxManifestSize bytes are read.
func (z *Zot) ManifestBody(ctx context.Context, repo, ref string) ([]byte, error) {
	r, err := z.req(ctx, http.MethodGet, objPath(repo, kindManifests, ref), nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := z.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxManifestSize {
			return nil, fmt.Errorf("manifest %s of %s is over %d bytes", ref, repo, maxManifestSize)
		}
		return b, nil
	case http.StatusNotFound:
		return nil, nil
	}
	return nil, fmt.Errorf("zot answered %d to GET manifest %s of %s", resp.StatusCode, ref, repo)
}

// DeleteManifest deletes a manifest by digest; a missing one is fine.
func (z *Zot) DeleteManifest(ctx context.Context, repo, digest string) error {
	return z.delete(ctx, objPath(repo, kindManifests, digest))
}

// DeleteBlob removes the link of a blob in one repository; a missing one is fine, and so is a zot that refuses (its garbage collection
// then does it).
func (z *Zot) DeleteBlob(ctx context.Context, repo, digest string) error {
	return z.delete(ctx, objPath(repo, kindBlobs, digest))
}

func (z *Zot) delete(ctx context.Context, path string) error {
	r, err := z.req(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	resp, err := z.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK, http.StatusNoContent, http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil
	}
	return fmt.Errorf("zot answered %d to DELETE %s", resp.StatusCode, path)
}

// Repos lists the repositories of zot (the catalog), following the pagination.
func (z *Zot) Repos(ctx context.Context) ([]string, error) {
	var out []string
	last := ""
	for page := 0; page < 1000; page++ {
		q := url.Values{"n": {"1000"}}
		if last != "" {
			q.Set("last", last)
		}
		r, err := z.req(ctx, http.MethodGet, "/v2/_catalog", nil)
		if err != nil {
			return nil, err
		}
		r.URL.RawQuery = q.Encode()
		resp, err := z.HTTP.Do(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Repositories []string `json:"repositories"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body)
		status := resp.StatusCode
		drain(resp)
		if status != http.StatusOK || err != nil {
			return nil, fmt.Errorf("zot answered %d to the catalog (%v)", status, err)
		}
		if len(body.Repositories) == 0 {
			return out, nil
		}
		out = append(out, body.Repositories...)
		last = body.Repositories[len(body.Repositories)-1]
		if len(body.Repositories) < 1000 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("the catalog of zot has too many pages")
}

// Tags lists the tags of a repository (the first page of at most 1000; a snapshot repository has one tag).
func (z *Zot) Tags(ctx context.Context, repo string) ([]string, error) {
	r, err := z.req(ctx, http.MethodGet, "/v2/"+repo+"/tags/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := z.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body)
	status := resp.StatusCode
	drain(resp)
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK || err != nil {
		return nil, fmt.Errorf("zot answered %d to the tag list of %s (%v)", status, repo, err)
	}
	return body.Tags, nil
}
