// Package granttest signs grants for tests. The laboratory never signs a grant (the platform of a tenant does); this package must not be
// imported by production code.
package granttest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cybericebox/laboratory/internal/grant"
)

// Key is a tenant's access key pair under a key id.
type Key struct {
	Tenant, ID string
	Public     ed25519.PublicKey
	Private    ed25519.PrivateKey
}

// MustKey makes the key of a package-level variable of a test. It is derived from the tenant and the id, so it is the same in every process of
// a test run: a test that starts the code under test as a child process (the proxy memory test) signs in the parent and verifies in the child.
// It is a test key and never a secret.
func MustKey(tenant, id string) Key {
	seed := sha256.Sum256([]byte("granttest/" + tenant + "/" + id))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return Key{Tenant: tenant, ID: id, Public: priv.Public().(ed25519.PublicKey), Private: priv}
}

// NewKey makes a key.
func NewKey(t testing.TB, tenant, id string) Key {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Key{Tenant: tenant, ID: id, Public: pub, Private: priv}
}

// PEM is the PKIX PEM of the public key, as the access key Secret holds it.
func (k Key) PEM(t testing.TB) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(k.Public)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// Lookup is a grant.KeyLookup that knows only this key.
func (k Key) Lookup() grant.KeyLookup {
	return func(tenant, id string) (ed25519.PublicKey, bool) {
		if tenant == k.Tenant && id == k.ID {
			return k.Public, true
		}
		return nil, false
	}
}

// Claims returns valid web claims for a host, issued at now and valid for ttl. A test changes the field it wants to break.
func (k Key) Claims(now time.Time, ttl time.Duration, host string) grant.Claims {
	return grant.Claims{
		Group: "group-1", Lab: "lab1", Dev: "web", Host: host, Kind: grant.KindWeb,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: k.Tenant, Subject: "p-user1", Audience: jwt.ClaimStrings{grant.AudienceProxy},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
}

// Sign signs claims with the key, typ "cib-grant" and the kid of the key.
func (k Key) Sign(t testing.TB, c grant.Claims) string {
	t.Helper()
	return k.SignWith(t, c, jwt.SigningMethodEdDSA, k.ID, grant.Type)
}

// SignWith signs with an explicit method, kid and typ (to build a grant that must fail).
func (k Key) SignWith(t testing.TB, c grant.Claims, method jwt.SigningMethod, kid, typ string) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, c)
	tok.Header["kid"], tok.Header["typ"] = kid, typ
	var key any = k.Private
	if method.Alg() == "none" {
		key = jwt.UnsafeAllowNoneSignatureType
	}
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
