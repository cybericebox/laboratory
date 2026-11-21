package deviceaccess

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/grant"
	"github.com/cybericebox/laboratory/internal/grant/granttest"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

const grantHost = "dev-abc12t.labs.example.test"

// consoleClaims are the claims of a console grant for the request openReq makes.
func consoleClaims(k granttest.Key, now time.Time) grant.Claims {
	c := k.Claims(now, time.Hour, grantHost)
	c.Group, c.Lab, c.Dev, c.Kind, c.Subject = "g-1", testLab, testDev, grant.KindConsole, "c-1"
	c.Audience = jwt.ClaimStrings{grant.AudienceProxy, grant.AudienceDeviceAccess}
	return c
}

func withGrant(t *testing.T, k granttest.Key, c grant.Claims) wire.OpenRequest {
	t.Helper()
	r := openReq()
	r.Grant = k.Sign(t, c)
	return r
}

func authAt(now *time.Time, k granttest.Key) *Auth {
	a := NewAuth(k.Lookup(), 2*time.Hour)
	a.Now = func() time.Time { return *now }
	return a
}

func TestAuthCheckAcceptsAGrantThatAgreesWithTheRequest(t *testing.T) {
	now := t0
	k := granttest.NewKey(t, "acme", "k1")
	claims, err := authAt(&now, k).Check(withGrant(t, k, consoleClaims(k, now)))
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if claims.Client() != "c-1" || claims.Group != "g-1" || !claims.End().Equal(now.Add(time.Hour)) {
		t.Fatalf("claims = %+v", claims)
	}
}

// Every row of the claim table of the spec as a failing case, as the service sees it.
func TestAuthCheckRefusesABrokenGrant(t *testing.T) {
	now := t0
	k := granttest.NewKey(t, "acme", "k1")
	other := granttest.NewKey(t, "acme", "k1") // another key pair under the same ids
	cases := map[string]wire.OpenRequest{
		"no grant":    openReq(),
		"garbage":     func() wire.OpenRequest { r := openReq(); r.Grant = "a.b.c"; return r }(),
		"another key": withGrant(t, other, consoleClaims(k, now)),
		"expired":     withGrant(t, k, k.Claims(now.Add(-3*time.Hour), time.Hour, grantHost)),
		"not yet valid": withGrant(t, k, func() grant.Claims {
			c := consoleClaims(k, now)
			c.NotBefore = jwt.NewNumericDate(now.Add(time.Hour))
			return c
		}()),
		"lives too long": withGrant(t, k, func() grant.Claims {
			c := consoleClaims(k, now)
			c.ExpiresAt = jwt.NewNumericDate(now.Add(3 * time.Hour))
			return c
		}()),
		"a web grant": withGrant(t, k, func() grant.Claims { c := consoleClaims(k, now); c.Kind = grant.KindWeb; return c }()),
		"proxy audience": withGrant(t, k, func() grant.Claims {
			c := consoleClaims(k, now)
			c.Audience = jwt.ClaimStrings{grant.AudienceProxy}
			return c
		}()),
		"no audience":    withGrant(t, k, func() grant.Claims { c := consoleClaims(k, now); c.Audience = nil; return c }()),
		"unknown tenant": withGrant(t, k, func() grant.Claims { c := consoleClaims(k, now); c.Issuer = "mallory"; return c }()),
		"no subject":     withGrant(t, k, func() grant.Claims { c := consoleClaims(k, now); c.Subject = ""; return c }()),
		"no group":       withGrant(t, k, func() grant.Claims { c := consoleClaims(k, now); c.Group = ""; return c }()),
		"no lab":         withGrant(t, k, func() grant.Claims { c := consoleClaims(k, now); c.Lab = ""; return c }()),
		"no device":      withGrant(t, k, func() grant.Claims { c := consoleClaims(k, now); c.Dev = ""; return c }()),
		"no host":        withGrant(t, k, func() grant.Claims { c := consoleClaims(k, now); c.Host = ""; return c }()),
		"unknown key id": func() wire.OpenRequest {
			r := openReq()
			r.Grant = k.SignWith(t, consoleClaims(k, now), jwt.SigningMethodEdDSA, "k9", grant.Type)
			return r
		}(),
		"alg none": func() wire.OpenRequest {
			r := openReq()
			r.Grant = k.SignWith(t, consoleClaims(k, now), jwt.SigningMethodNone, "k1", grant.Type)
			return r
		}(),
		"another grant typ": func() wire.OpenRequest {
			r := openReq()
			r.Grant = k.SignWith(t, consoleClaims(k, now), jwt.SigningMethodEdDSA, "k1", "JWT")
			return r
		}(),
	}
	for name, r := range cases {
		if _, err := authAt(&now, k).Check(r); err == nil {
			t.Errorf("%s: a broken grant was accepted", name)
		}
	}
}

// The open request must agree with the grant: a proxy (or a node that reaches the service) cannot ask for another device, group or client than
// the one the platform authorised.
func TestAuthCheckRefusesARequestThatDisagreesWithItsGrant(t *testing.T) {
	now := t0
	k := granttest.NewKey(t, "acme", "k1")
	mods := map[string]func(*wire.OpenRequest){
		"client": func(r *wire.OpenRequest) { r.Client = "c-2" },
		"group":  func(r *wire.OpenRequest) { r.Group = "g-2" },
		"lab":    func(r *wire.OpenRequest) { r.Lab = "other" },
		"device": func(r *wire.OpenRequest) { r.Device = "other" },
	}
	for name, mod := range mods {
		r := withGrant(t, k, consoleClaims(k, now))
		mod(&r)
		if _, err := authAt(&now, k).Check(r); err != errMismatch {
			t.Errorf("%s: %v", name, err)
		}
	}
	// what the grant does not name (the settings, the size, the namespace) is not part of the agreement
	r := withGrant(t, k, consoleClaims(k, now))
	r.Cols, r.Rows, r.User, r.Command = 120, 40, "root", []string{"/bin/bash"}
	if _, err := authAt(&now, k).Check(r); err != nil {
		t.Errorf("the terminal settings are checked elsewhere: %v", err)
	}
}

func TestAuthWithoutKeysRefuses(t *testing.T) {
	now := t0
	k := granttest.NewKey(t, "acme", "k1")
	r := withGrant(t, k, consoleClaims(k, now))
	if _, err := (&Auth{}).Check(r); err == nil {
		t.Error("accepted with no key lookup")
	}
	var nilAuth *Auth
	if _, err := nilAuth.Check(r); err == nil {
		t.Error("a nil Auth accepted")
	}
	none := NewAuth(func(string, string) (ed25519.PublicKey, bool) { return nil, false }, time.Hour)
	if _, err := none.Check(r); err == nil {
		t.Error("accepted with a lookup that knows no key")
	}
}

// A grant is a bearer: the same grant opens again (no nonce, no replay memory), until its exp.
func TestAuthCheckHasNoReplayMemory(t *testing.T) {
	now := t0
	k := granttest.NewKey(t, "acme", "k1")
	a := authAt(&now, k)
	r := withGrant(t, k, consoleClaims(k, now))
	for i := 0; i < 3; i++ {
		if _, err := a.Check(r); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	now = now.Add(2 * time.Hour)
	if _, err := a.Check(r); err == nil {
		t.Fatal("after exp")
	}
}
