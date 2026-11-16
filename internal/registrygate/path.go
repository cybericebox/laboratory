// Package registrygate is the gate in front of the platform registry (zot): the one component every request to the registry passes. See
// docs/research/2026-11-16-zot-tenant-cache.md. This part is the pass-through with the request hygiene the cache path will rely on: zot
// listens on the loopback of the pod only, and nothing reaches it that the gate did not accept.
package registrygate

import (
	"errors"
	"strings"
)

// MaxPathLength bounds the path of a request (an OCI repository name is at most 255 characters; a digest, a tag and the fixed words
// come on top).
const MaxPathLength = 1024

// errPath is why a path is refused.
var errPath = errors.New("registry path refused")

// checkPath accepts the path of a request to the registry API and refuses everything that could be read two ways by two components
// (the gate and zot): dot segments, empty segments, encoded slashes and backslashes, control characters, NUL, percent signs that do not
// decode to a harmless byte, and anything outside /v2. escaped is the path as the client sent it (URL.EscapedPath): the check is made on
// that form, before anyone decodes it, and the same string is what is forwarded.
func checkPath(escaped string) error {
	if escaped == "/v2" || escaped == "/v2/" {
		return nil
	}
	if len(escaped) > MaxPathLength || !strings.HasPrefix(escaped, "/v2/") {
		return errPath
	}
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		switch {
		case c < 0x21 || c == 0x7f || c == '\\' || c == '?' || c == '#':
			return errPath
		case c == '%':
			if i+2 >= len(escaped) {
				return errPath
			}
			hi, lo := unhex(escaped[i+1]), unhex(escaped[i+2])
			if hi < 0 || lo < 0 {
				return errPath
			}
			// an encoded byte may be a letter, a digit or one of -_~ and nothing else: no slash, dot, percent or control character is
			// ever hidden in an escape
			b := byte(hi<<4 | lo)
			if !unreserved(b) || b == '.' {
				return errPath
			}
			i += 2
		}
	}
	rest := strings.TrimPrefix(escaped, "/v2/")
	// the one trailing slash the API has: POST /v2/<name>/blobs/uploads/
	if strings.HasSuffix(rest, "/blobs/uploads/") {
		rest = strings.TrimSuffix(rest, "/")
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errPath
		}
	}
	return nil
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func unreserved(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '.' || b == '_' || b == '~'
}
