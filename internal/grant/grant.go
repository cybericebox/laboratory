// Package grant is the access grant: one compact JWS the platform of a tenant signs for one device host, and the only credential a
// browser holds for a web or console session. Nothing is signed by the laboratory: the L7 proxy and the device access service only verify
// the grant against the access public key the tenant registered (see KeyLookup).
//
// Format (docs/laboratory/specs/pools-release.md, 2.3.1): EdDSA, header {"alg":"EdDSA","kid":<key id>,"typ":"cib-grant"}, claims iss (the
// tenant), sub (the LabGroupClient), aud, group_id, lab, dev, host (the full access host, lower case), kind ("web" or "console"), iat, nbf,
// exp. There is no session id, no replay memory and no user or event id: the grant is a bearer bound to one host.
package grant

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// Type is the typ header of a grant.
	Type = "cib-grant"
	// AudienceProxy is the aud every grant carries: the L7 proxy verifies it. A console grant carries AudienceDeviceAccess as well, so the
	// device access service can verify the same grant on the node.
	AudienceProxy        = "laboratory-proxy"
	AudienceDeviceAccess = "laboratory-device-access"

	// KindWeb is the grant of a web host (a cookie is set from it); KindConsole is the grant of a console host (it opens a stream).
	KindWeb     = "web"
	KindConsole = "console"

	// Leeway is the clock difference between the platform and the verifier that is tolerated on iat, nbf and exp.
	Leeway = 30 * time.Second
	// MaxLen is the longest accepted grant (the form field and the cookie hold it).
	MaxLen = 4096
	// DefaultMaxTTL is the longest exp - iat the verifiers accept unless configured otherwise (chart proxy.l7.grantMaxTTL).
	DefaultMaxTTL = 8 * time.Hour
)

// Claims are the claims of a grant.
type Claims struct {
	// Group is the platform's id of the LabGroup (the proxy maps it with names.EncodeName).
	Group string `json:"group_id"`
	// Lab is the Lab name and Dev the device name (Device.Spec.Name) the host is for.
	Lab string `json:"lab"`
	Dev string `json:"dev"`
	// Host is the FULL access host (<device>-<code><kind letter>.<base domain>), lower case, no port. A request is served only for exactly this host.
	Host string `json:"host"`
	// Kind is KindWeb or KindConsole and agrees with the kind letter of Host.
	Kind string `json:"kind"`
	jwt.RegisteredClaims
}

// Client is the LabGroupClient of the grant (sub).
func (c Claims) Client() string { return c.Subject }

// Tenant is the tenant that signed the grant (iss).
func (c Claims) Tenant() string { return c.Issuer }

// End is exp.
func (c Claims) End() time.Time { return c.ExpiresAt.Time }

// KeyLookup finds the access public key a tenant registered under a key id.
type KeyLookup func(tenant, keyID string) (ed25519.PublicKey, bool)

// Errors of Verify. Callers never tell a client which one it was.
var (
	ErrInvalid = errors.New("invalid grant")
)

var tenantNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidTenant says whether s can be a tenant name (a DNS-1123 label of at most 63 characters).
func ValidTenant(s string) bool { return len(s) <= 63 && tenantNameRE.MatchString(s) }

// Options say what a verifier requires of a grant.
type Options struct {
	// Keys finds the key of (iss, kid). Required.
	Keys KeyLookup
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// MaxTTL bounds exp - iat; zero means DefaultMaxTTL.
	MaxTTL time.Duration
	// Audience is the aud the grant must contain.
	Audience string
	// Kind is the kind the grant must have.
	Kind string
	// Host, when not empty, is the host the grant must have been issued for (lower case, no port).
	Host string
}

// Verify checks a grant: EdDSA only, the key of (iss, kid), typ, aud, iat/nbf/exp with the lifetime bound, the kind, the host (when
// asked) and that the claims every request needs are present. It does not look at the group: the caller checks that the group of the
// grant belongs to the issuer.
func Verify(raw string, o Options) (Claims, error) {
	if raw == "" || len(raw) > MaxLen || o.Keys == nil {
		return Claims{}, ErrInvalid
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	max := o.MaxTTL
	if max <= 0 {
		max = DefaultMaxTTL
	}
	var claims Claims
	token, err := jwt.ParseWithClaims(raw, &claims, keyFunc(o.Keys),
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithTimeFunc(now), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithAudience(o.Audience), jwt.WithLeeway(Leeway),
	)
	if err != nil || !token.Valid {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if typ, _ := token.Header["typ"].(string); typ != Type {
		return Claims{}, fmt.Errorf("%w: typ", ErrInvalid)
	}
	if claims.Group == "" || claims.Subject == "" || claims.Lab == "" || claims.Dev == "" || claims.Host == "" {
		return Claims{}, fmt.Errorf("%w: incomplete", ErrInvalid)
	}
	if claims.IssuedAt == nil || claims.NotBefore == nil || claims.ExpiresAt == nil {
		return Claims{}, fmt.Errorf("%w: iat, nbf and exp are required", ErrInvalid)
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt.Time) > max {
		return Claims{}, fmt.Errorf("%w: lives too long", ErrInvalid)
	}
	if claims.Kind != o.Kind || (claims.Kind != KindWeb && claims.Kind != KindConsole) {
		return Claims{}, fmt.Errorf("%w: kind", ErrInvalid)
	}
	if o.Host != "" && claims.Host != o.Host {
		return Claims{}, fmt.Errorf("%w: another host", ErrInvalid)
	}
	return claims, nil
}

// keyFunc finds the key a tenant's grant is signed with: EdDSA only, iss names the tenant and the header kid the key of that tenant.
func keyFunc(keys KeyLookup) jwt.Keyfunc {
	return func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		var issuer string
		if c, ok := t.Claims.(*Claims); ok {
			issuer = c.Issuer
		}
		if kid == "" || !ValidTenant(issuer) {
			return nil, errors.New("no issuer or key id")
		}
		key, ok := keys(issuer, kid)
		if !ok {
			return nil, errors.New("unknown issuer or key id")
		}
		return key, nil
	}
}

// ParseAccessKey reads a PKIX PEM Ed25519 public key.
func ParseAccessKey(data []byte) (ed25519.PublicKey, bool) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, false
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, false
	}
	k, ok := pub.(ed25519.PublicKey)
	return k, ok
}
