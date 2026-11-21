package grant_test

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cybericebox/laboratory/internal/grant"
	"github.com/cybericebox/laboratory/internal/grant/granttest"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

const host = "web-abc12w.labs.example.test"

func opts(k granttest.Key) grant.Options {
	return grant.Options{Keys: k.Lookup(), Now: func() time.Time { return t0 }, MaxTTL: time.Hour, Audience: grant.AudienceProxy, Kind: grant.KindWeb, Host: host}
}

func TestVerifyAcceptsAValidGrant(t *testing.T) {
	k := granttest.NewKey(t, "acme", "k1")
	c, err := grant.Verify(k.Sign(t, k.Claims(t0, time.Hour, host)), opts(k))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tenant() != "acme" || c.Client() != "p-user1" || c.Group != "group-1" || c.Lab != "lab1" || c.Dev != "web" || !c.End().Equal(t0.Add(time.Hour)) {
		t.Fatalf("claims: %+v", c)
	}
}

// Every row of the claim table of the spec (2.3.1) as a failing case.
func TestVerifyRefusesEveryBrokenClaim(t *testing.T) {
	k := granttest.NewKey(t, "acme", "k1")
	other := granttest.NewKey(t, "acme", "k1") // the same ids, another key pair: a bad signature
	good := func() grant.Claims { return k.Claims(t0, time.Hour, host) }
	for name, raw := range map[string]string{
		"bad signature":     other.Sign(t, good()),
		"unknown kid":       k.SignWith(t, good(), jwt.SigningMethodEdDSA, "k2", grant.Type),
		"no kid":            k.SignWith(t, good(), jwt.SigningMethodEdDSA, "", grant.Type),
		"wrong typ":         k.SignWith(t, good(), jwt.SigningMethodEdDSA, "k1", "JWT"),
		"alg none":          k.SignWith(t, good(), jwt.SigningMethodNone, "k1", grant.Type),
		"unknown tenant":    k.Sign(t, with(good(), func(c *grant.Claims) { c.Issuer = "other" })),
		"bad issuer":        k.Sign(t, with(good(), func(c *grant.Claims) { c.Issuer = "Acme_" })),
		"empty issuer":      k.Sign(t, with(good(), func(c *grant.Claims) { c.Issuer = "" })),
		"no subject":        k.Sign(t, with(good(), func(c *grant.Claims) { c.Subject = "" })),
		"wrong aud":         k.Sign(t, with(good(), func(c *grant.Claims) { c.Audience = jwt.ClaimStrings{"laboratory-device-access"} })),
		"no aud":            k.Sign(t, with(good(), func(c *grant.Claims) { c.Audience = nil })),
		"no group":          k.Sign(t, with(good(), func(c *grant.Claims) { c.Group = "" })),
		"no lab":            k.Sign(t, with(good(), func(c *grant.Claims) { c.Lab = "" })),
		"no dev":            k.Sign(t, with(good(), func(c *grant.Claims) { c.Dev = "" })),
		"no host":           k.Sign(t, with(good(), func(c *grant.Claims) { c.Host = "" })),
		"another host":      k.Sign(t, with(good(), func(c *grant.Claims) { c.Host = "db-abc12w.labs.example.test" })),
		"console kind":      k.Sign(t, with(good(), func(c *grant.Claims) { c.Kind = grant.KindConsole })),
		"unknown kind":      k.Sign(t, with(good(), func(c *grant.Claims) { c.Kind = "vnc" })),
		"no kind":           k.Sign(t, with(good(), func(c *grant.Claims) { c.Kind = "" })),
		"expired":           k.Sign(t, k.Claims(t0.Add(-2*time.Hour), time.Hour, host)),
		"not yet valid":     k.Sign(t, k.Claims(t0.Add(time.Hour), time.Hour, host)),
		"nbf in the future": k.Sign(t, with(good(), func(c *grant.Claims) { c.NotBefore = jwt.NewNumericDate(t0.Add(time.Minute)) })),
		"no iat":            k.Sign(t, with(good(), func(c *grant.Claims) { c.IssuedAt = nil })),
		"no nbf":            k.Sign(t, with(good(), func(c *grant.Claims) { c.NotBefore = nil })),
		"no exp":            k.Sign(t, with(good(), func(c *grant.Claims) { c.ExpiresAt = nil })),
		"lives too long":    k.Sign(t, k.Claims(t0, time.Hour+time.Second, host)),
		"empty":             "",
		"garbage":           "not.a.grant",
		"too long":          strings.Repeat("a", grant.MaxLen+1),
	} {
		if _, err := grant.Verify(raw, opts(k)); err == nil {
			t.Errorf("%s: a broken grant was accepted", name)
		}
	}
}

func TestVerifyToleratesTheClockOnlyByTheLeeway(t *testing.T) {
	k := granttest.NewKey(t, "acme", "k1")
	if _, err := grant.Verify(k.Sign(t, k.Claims(t0.Add(20*time.Second), time.Hour, host)), opts(k)); err != nil {
		t.Fatalf("a grant issued 20s ahead of this clock is within the leeway: %v", err)
	}
	if _, err := grant.Verify(k.Sign(t, k.Claims(t0.Add(time.Minute), time.Hour, host)), opts(k)); err == nil {
		t.Fatal("a grant issued a minute ahead is refused")
	}
	if _, err := grant.Verify(k.Sign(t, k.Claims(t0.Add(-time.Hour-20*time.Second), time.Hour, host)), opts(k)); err == nil {
		t.Fatal("a grant that ended 20s ago is refused: the leeway does not extend exp")
	}
}

func TestVerifyConsoleNeedsBothAudiencesToServeBothVerifiers(t *testing.T) {
	k := granttest.NewKey(t, "acme", "k1")
	c := k.Claims(t0, time.Hour, "sh-abc12t.labs.example.test")
	c.Kind = grant.KindConsole
	c.Audience = jwt.ClaimStrings{grant.AudienceProxy, grant.AudienceDeviceAccess}
	raw := k.Sign(t, c)
	for _, aud := range []string{grant.AudienceProxy, grant.AudienceDeviceAccess} {
		o := opts(k)
		o.Kind, o.Audience, o.Host = grant.KindConsole, aud, ""
		if _, err := grant.Verify(raw, o); err != nil {
			t.Errorf("aud %s: %v", aud, err)
		}
	}
}

func with(c grant.Claims, f func(*grant.Claims)) grant.Claims { f(&c); return c }

// The key is looked up by the issuer the grant names: the key id of tenant B does not verify a grant whose iss says tenant A, even when
// both tenants use the same key id.
func TestAKeyOfAnotherTenantDoesNotVerifyAGrantOfThisOne(t *testing.T) {
	acme, beta := granttest.NewKey(t, "acme", "k1"), granttest.NewKey(t, "beta", "k1")
	lookup := func(tenant, id string) (ed25519.PublicKey, bool) {
		for _, k := range []granttest.Key{acme, beta} {
			if k.Tenant == tenant && k.ID == id {
				return k.Public, true
			}
		}
		return nil, false
	}
	o := opts(acme)
	o.Keys = lookup
	// signed by beta, claiming to be acme (iss acme, kid k1): the key looked up is acme's
	c := beta.Claims(t0, time.Hour, host)
	c.Issuer = "acme"
	if _, err := grant.Verify(beta.Sign(t, c), o); err == nil {
		t.Fatal("beta's signature verified as acme's grant")
	}
	// signed by beta as beta: valid, and the claims say beta (the caller binds that to the tenant of the object)
	got, err := grant.Verify(beta.Sign(t, beta.Claims(t0, time.Hour, host)), o)
	if err != nil || got.Tenant() != "beta" {
		t.Fatalf("beta's own grant: %v %+v", err, got)
	}
}

// A grant is valid until its exp exactly: the leeway is for the clock of the issuer ahead (nbf, iat), never for a grant that has ended.
func TestVerifyRefusesAGrantAtItsExp(t *testing.T) {
	k := granttest.NewKey(t, "acme", "k1")
	raw := k.Sign(t, k.Claims(t0.Add(-time.Hour), time.Hour, host)) // exp == t0
	if _, err := grant.Verify(raw, opts(k)); err == nil {
		t.Fatal("a grant verified at its exp")
	}
	raw = k.Sign(t, k.Claims(t0.Add(-time.Hour+time.Second), time.Hour, host)) // exp == t0 + 1s
	if _, err := grant.Verify(raw, opts(k)); err != nil {
		t.Fatalf("a grant one second before its exp: %v", err)
	}
}
