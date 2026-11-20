package registrygate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/cybericebox/laboratory/internal/names"
)

// imageRef is a reference in a pod, as the operator writes it: <prefix>/c/<segment>/<registry>/<repo>[:tag][@sha256:<digest>].
type imageRef struct {
	Host     string
	Seg      string
	Registry string
	Repo     string
	Tag      string
	Digest   string
}

func (i imageRef) fullRepo() string { return i.Registry + "/" + i.Repo }

// parseCacheImage reads a container image string of a pod; ok is false for an image that does not go through the cache.
func parseCacheImage(image string) (imageRef, bool) {
	i := strings.Index(image, "/c/")
	if i <= 0 || strings.Contains(image[:i], "/") {
		return imageRef{}, false // the cache is addressed as <host>/c/...: anything with another path in front is not it
	}
	rest := image[i+len("/c/"):]
	out := imageRef{Host: image[:i]}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		out.Digest, rest = rest[at+1:], rest[:at]
	}
	segs := strings.Split(rest, "/")
	if len(segs) < 3 {
		return imageRef{}, false
	}
	if last := segs[len(segs)-1]; strings.Contains(last, ":") {
		last, out.Tag, _ = strings.Cut(last, ":")
		segs[len(segs)-1] = last
	}
	out.Seg, out.Registry, out.Repo = segs[0], segs[1], strings.Join(segs[2:], "/")
	if out.Seg == "" || out.Registry == "" || out.Repo == "" {
		return imageRef{}, false
	}
	return out, true
}

// parseImage is parseCacheImage that also checks the host when the gate knows the one the nodes pull from (ImageHost).
func (c *Cache) parseImage(image string) (imageRef, bool) {
	ref, ok := parseCacheImage(image)
	if ok && c.ImageHost != "" && ref.Host != c.ImageHost {
		return imageRef{}, false
	}
	return ref, ok
}

// matches says whether a request for ref of o is exactly what the reference names: a digest-pinned image allows that digest only, a tag
// image that tag only (an image with neither is the tag "latest").
func (i imageRef) matches(o objectRef) bool {
	if i.Seg != o.Tenant || i.Registry != o.Registry || i.Repo != o.Repo {
		return false
	}
	if i.Digest != "" {
		return o.Ref == i.Digest
	}
	tag := i.Tag
	if tag == "" {
		tag = "latest"
	}
	return o.Ref == tag
}

// podOfTenant says whether a pod belongs to the tenant of the segment: a pod of a group namespace of that tenant, or, for the segment
// "platform", a VPN or gateway pod of any group.
func (c *Cache) podOfTenant(p PodInfo, seg, tenant string) bool {
	groupTenant, isGroup := c.Schedule.TenantOfNamespace(p.Namespace)
	if !isGroup {
		return false
	}
	if seg == names.SegmentPlatform {
		comp := p.Labels[names.LabelComponent]
		return comp == names.ComponentVPN || comp == names.ComponentGateway
	}
	return groupTenant == tenant
}

// scheduleMatch is step 3 of 2.4.2 for a request of a node: some pod bound to the node has a container or init container whose image is
// exactly this reference and belongs to the tenant of the segment, or an ImagePull lists the node, this reference and this tenant. It
// returns the labs whose pods name the reference (their own credentials may be needed).
func (c *Cache) scheduleMatch(node, tenant string, o objectRef) ([]LabRef, bool) {
	var labs []LabRef
	seen := map[LabRef]bool{}
	ok := false
	for _, pod := range c.Schedule.PodsOnNode(node) {
		if pod.Ended() { // a pod that finished or is going away pulls nothing more
			continue
		}
		for _, img := range pod.Images {
			ref, isCache := c.parseImage(img)
			if !isCache || !ref.matches(o) || !c.podOfTenant(pod, o.Tenant, tenant) {
				continue
			}
			ok = true
			if lab := pod.Labels[names.LabelLab]; lab != "" {
				l := LabRef{Namespace: pod.Namespace, Lab: lab}
				if !seen[l] {
					seen[l] = true
					labs = append(labs, l)
				}
			}
		}
	}
	for _, ip := range c.Schedule.ImagePullsForNode(node) {
		if o.Tenant != names.SegmentPlatform && names.TenantSegment(ip.Tenant) != o.Tenant {
			continue
		}
		if o.Tenant == names.SegmentPlatform && ip.Tenant != names.SegmentPlatform {
			continue
		}
		for _, img := range ip.Images {
			if ref, isCache := c.parseImage(img); isCache && ref.matches(o) {
				ok = true
			}
		}
	}
	return labs, ok
}

// anyPodFor says whether some pod (or ImagePull) on the node names the repository of o for the tenant of the segment, with any tag or
// digest: a blob request for a repository nothing on the node names is refused outright.
func (c *Cache) anyPodFor(node, tenant string, o objectRef) bool {
	for _, pod := range c.Schedule.PodsOnNode(node) {
		if pod.Ended() { // a pod that finished or is going away pulls nothing more
			continue
		}
		for _, img := range pod.Images {
			ref, isCache := c.parseImage(img)
			if isCache && ref.Seg == o.Tenant && ref.fullRepo() == o.fullRepo() && c.podOfTenant(pod, o.Tenant, tenant) {
				return true
			}
		}
	}
	for _, ip := range c.Schedule.ImagePullsForNode(node) {
		if (o.Tenant == names.SegmentPlatform && ip.Tenant != names.SegmentPlatform) || (o.Tenant != names.SegmentPlatform && names.TenantSegment(ip.Tenant) != o.Tenant) {
			continue
		}
		for _, img := range ip.Images {
			if ref, isCache := c.parseImage(img); isCache && ref.Seg == o.Tenant && ref.fullRepo() == o.fullRepo() {
				return true
			}
		}
	}
	return false
}

// childDigests are the digests a manifest lists: the child manifests of an index, the config and layers of an image manifest.
func childDigests(body []byte) []string {
	var m struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	var out []string
	for _, c := range m.Manifests {
		out = append(out, c.Digest)
	}
	if m.Config.Digest != "" {
		out = append(out, m.Config.Digest)
	}
	for _, l := range m.Layers {
		out = append(out, l.Digest)
	}
	return out
}

func sha256Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}
