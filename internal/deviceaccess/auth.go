package deviceaccess

import (
	"errors"
	"time"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/grant"
)

// Auth checks the grant of an open request. Check is the one place where an open request is authenticated; it runs before anything else
// is done with the request. The grant is the console grant the tenant's platform signed (internal/grant); the service verifies it with the
// tenant's access public key and holds no key of its own, so a rooted node cannot make one.
type Auth struct {
	// Keys finds the access key of (tenant, key id): the keys the operator mirrors into the service's ConfigMap (grant.DirKeys).
	Keys grant.KeyLookup
	// MaxTTL is the longest exp - iat accepted (the chart's terminal.grantMaxTTL); zero means grant.DefaultMaxTTL.
	MaxTTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

var (
	errUnsigned = errors.New("the grant of the request is not valid")
	errMismatch = errors.New("the request does not agree with its grant")
)

// NewAuth returns an Auth over a key lookup.
func NewAuth(keys grant.KeyLookup, maxTTL time.Duration) *Auth {
	return &Auth{Keys: keys, MaxTTL: maxTTL}
}

// Check authenticates a request: a console grant for this service (aud laboratory-device-access), valid now, lived no longer than MaxTTL,
// and the open request agrees with it: client with sub, group with group_id, lab, and device with dev. It returns the claims; the stream
// ends at their exp. A failure never says which check failed.
func (a *Auth) Check(r wire.OpenRequest) (grant.Claims, error) {
	if a == nil || a.Keys == nil {
		return grant.Claims{}, errUnsigned
	}
	claims, err := grant.Verify(r.Grant, grant.Options{
		Keys: a.Keys, Now: a.Now, MaxTTL: a.MaxTTL, Audience: grant.AudienceDeviceAccess, Kind: grant.KindConsole,
	})
	if err != nil {
		return grant.Claims{}, errUnsigned
	}
	if r.Client != claims.Client() || r.Group != claims.Group || r.Lab != claims.Lab || r.Device != claims.Dev {
		return grant.Claims{}, errMismatch
	}
	return claims, nil
}
