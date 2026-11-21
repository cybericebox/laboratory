package registrygate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

// errBlobDigest: the bytes an upstream sent do not hash to the digest they were asked for.
var errBlobDigest = errors.New("the upstream blob does not match its digest")

// copyVerified copies src to dst and checks the sha256 of everything copied against want ("sha256:<hex>") while streaming. The LAST byte is
// held back until the stream has ended and the hash is known: on a mismatch it is never written, so a receiver that expects the whole
// length (the monolithic upload to zot, the client of the gate) never gets a complete body of wrong content. It returns the bytes read from
// src (the held byte included) and errBlobDigest on a mismatch.
func copyVerified(dst io.Writer, src io.Reader, want string) (int64, error) {
	h := sha256.New()
	buf := make([]byte, 32<<10)
	var total int64
	var held byte
	have := false
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			total += int64(n)
			h.Write(buf[:n])
			if have {
				if _, err := dst.Write([]byte{held}); err != nil {
					return total, err
				}
			}
			if _, err := dst.Write(buf[:n-1]); err != nil {
				return total, err
			}
			held, have = buf[n-1], true
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return total, rerr
		}
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != want {
		return total, errBlobDigest
	}
	if have {
		if _, err := dst.Write([]byte{held}); err != nil {
			return total, err
		}
	}
	return total, nil
}
