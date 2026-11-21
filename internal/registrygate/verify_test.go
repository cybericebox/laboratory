package registrygate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestCopyVerified(t *testing.T) {
	data := bytes.Repeat([]byte("abc"), 50000) // more than one read buffer
	sum := sha256.Sum256(data)
	want := "sha256:" + hex.EncodeToString(sum[:])

	var ok bytes.Buffer
	if n, err := copyVerified(&ok, bytes.NewReader(data), want); err != nil || n != int64(len(data)) || !bytes.Equal(ok.Bytes(), data) {
		t.Fatalf("a right stream: %d %v", n, err)
	}
	var bad bytes.Buffer
	_, err := copyVerified(&bad, bytes.NewReader(data), "sha256:"+strings.Repeat("0", 64))
	if !errors.Is(err, errBlobDigest) || bad.Len() != len(data)-1 {
		t.Fatalf("a wrong stream must stop one byte short: %v, %d of %d", err, bad.Len(), len(data))
	}
	var one bytes.Buffer
	s1 := sha256.Sum256([]byte("x"))
	if _, err := copyVerified(&one, strings.NewReader("x"), "sha256:"+hex.EncodeToString(s1[:])); err != nil || one.String() != "x" {
		t.Fatalf("one byte: %v %q", err, one.String())
	}
	var empty bytes.Buffer
	se := sha256.Sum256(nil)
	if n, err := copyVerified(&empty, strings.NewReader(""), "sha256:"+hex.EncodeToString(se[:])); err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
}
