package l7

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cybericebox/laboratory/internal/grant"
	"github.com/cybericebox/laboratory/internal/grant/granttest"
)

// labHost is the web host most tests open; the kind letter of its first label is w.
const labHost = "web-abc12w.challenges.example.com"

// testKey is the access key of tenant acme under the key id k1: the one key the tests' handlers know.
var testKey = granttest.MustKey("acme", "k1")

func testKeys() KeyLookup { return testKey.Lookup() }

// jwtClaims is the shape in which the tests describe the grant they mint: the group, the client and the host, with the tenant and expiry
// left to the defaults.
type jwtClaims struct {
	GroupID, Client, Tenant, Host string
	Lab, Dev, Kind                string
	Audience                      []string
	jwt.RegisteredClaims
}

// signCookie signs the grant of a web session. A grant names the one host it is for; a test that does not say defaults to labHost. The
// grant is issued an hour before its exp (or now, when that is in the future), so every test that sets only exp gets a valid lifetime.
func signCookie(t *testing.T, c jwtClaims) string {
	t.Helper()
	return testKey.Sign(t, claimsOf(c, time.Now()))
}

func claimsOf(c jwtClaims, now time.Time) grant.Claims {
	if c.Host == "" {
		c.Host = labHost
	}
	if c.Tenant == "" {
		c.Tenant = "acme"
	}
	if c.Lab == "" {
		c.Lab = "lab"
	}
	if c.Dev == "" {
		c.Dev = "web"
	}
	if c.Kind == "" {
		c.Kind = grant.KindWeb
	}
	aud := jwt.ClaimStrings{grant.AudienceProxy}
	if c.Audience != nil {
		aud = c.Audience
	}
	exp := now.Add(time.Hour)
	if c.ExpiresAt != nil {
		exp = c.ExpiresAt.Time
	}
	iat := exp.Add(-time.Hour)
	if iat.After(now) {
		iat = now
	}
	return grant.Claims{
		Group: c.GroupID, Lab: c.Lab, Dev: c.Dev, Host: c.Host, Kind: c.Kind,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: c.Tenant, Subject: c.Client, Audience: aud,
			IssuedAt: jwt.NewNumericDate(iat), NotBefore: jwt.NewNumericDate(iat), ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
}
