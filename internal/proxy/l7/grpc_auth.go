package l7

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultGRPCTokenMaxLifetime is the hard cap of exp - iat of a gRPC access credential. A caller that drives a device (the brain, for an AI
// opponent or the grader) holds it for its run, so it lives longer than a handoff link; it never lives longer than this.
const DefaultGRPCTokenMaxLifetime = 12 * time.Hour

const (
	// GRPCAudience is the aud of a gRPC access credential. It is not HandoffAudience: a handoff link is a one-click browser link, this is
	// a long-lived credential, and neither is accepted in the other's place.
	GRPCAudience = "laboratory-proxy-grpc"
	// GRPCTokenUse and GRPCKind are the values of the token_use and kind claims of a gRPC access credential.
	GRPCTokenUse = "grpc"
	GRPCKind     = "grpc"
	// GRPCAccessHeader is the metadata key that carries the credential. The proxy verifies it and removes it; every other header of the call
	// (authorization included) is the device's own and passes unchanged.
	GRPCAccessHeader = "X-Lab-Access"
)

// grpcClaims is the proxy credential of a non-browser client of the gRPC access of a device (EDA web-direct, programmatic level). It is
// signed like a handoff link (EdDSA, iss = the tenant, header kid = the access key id), and says: aud = GRPCAudience, token_use = kind =
// "grpc", sub = the LabGroupClient the calls act as, group_id = the LabGroup, host = the access host label of the device
// (<device>-<code>g) it is for, run = the run it belongs to, and iat, nbf, exp: exp ends every call and stream made with it.
type grpcClaims struct {
	GroupID  string `json:"group_id"`
	Host     string `json:"host"`
	TokenUse string `json:"token_use"`
	Kind     string `json:"kind"`
	// Run is the run (of an opponent or of the grader) the token was issued for; the proxy requires it and does not interpret it.
	Run string `json:"run"`
	jwt.RegisteredClaims
}

// GRPCGrant is what a verified gRPC credential allows: the group and client the calls act as, who issued it, the run, the moment it ends,
// and a check the live loop repeats (false: the credential is no longer good, e.g. the key that signed it was removed).
type GRPCGrant struct {
	Group, Client, Issuer, Run string
	Expires                    time.Time
	Valid                      func() bool
}

// GRPCVerifier decides whether the credential of a call to the host (the first DNS label) is good. It is the one place that knows who
// signs the credential and where the trust comes from, so the signer can be changed without touching the proxy path.
type GRPCVerifier interface {
	Verify(raw, host string, now func() time.Time) (GRPCGrant, error)
}

// tenantKeyVerifier verifies a credential signed by the platform with a tenant's access key, exactly like a handoff link: signature
// against the key of (iss, kid), aud, iat/nbf/exp, host, the group belongs to the issuer, with the lifetime cap of the kind.
type tenantKeyVerifier struct {
	keys        KeyLookup
	groupTenant GroupTenant
	maxLifetime time.Duration
}

func (v tenantKeyVerifier) Verify(raw, host string, now func() time.Time) (GRPCGrant, error) {
	var claims grpcClaims
	token, err := jwt.ParseWithClaims(
		raw, &claims, accessKeyFunc(v.keys),
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithTimeFunc(now), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithAudience(GRPCAudience), jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return GRPCGrant{}, fmt.Errorf("invalid credential: %w", err)
	}
	if !token.Valid {
		return GRPCGrant{}, fmt.Errorf("invalid credential")
	}
	// Exactly this audience: a token that is also a handoff link (or anything else) is refused.
	if len(claims.Audience) != 1 || claims.TokenUse != GRPCTokenUse || claims.Kind != GRPCKind {
		return GRPCGrant{}, fmt.Errorf("not a gRPC credential")
	}
	if claims.GroupID == "" || claims.Subject == "" || claims.Host == "" || claims.Run == "" {
		return GRPCGrant{}, fmt.Errorf("incomplete credential")
	}
	if claims.IssuedAt == nil || claims.NotBefore == nil || claims.ExpiresAt == nil {
		return GRPCGrant{}, fmt.Errorf("credential needs iat, nbf and exp")
	}
	max := v.maxLifetime
	if max <= 0 {
		max = DefaultGRPCTokenMaxLifetime
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt.Time) > max {
		return GRPCGrant{}, fmt.Errorf("credential lives too long")
	}
	if claims.Host != host {
		return GRPCGrant{}, fmt.Errorf("credential is for another host")
	}
	if v.groupTenant == nil {
		return GRPCGrant{}, fmt.Errorf("no way to tell the tenant of a group")
	}
	if owner, ok := v.groupTenant(claims.GroupID); !ok || owner != claims.Issuer {
		return GRPCGrant{}, fmt.Errorf("the group does not belong to the issuer")
	}
	kid, _ := token.Header["kid"].(string)
	issuer := claims.Issuer
	key, _ := v.keys(issuer, kid)
	return GRPCGrant{
		Group: claims.GroupID, Client: claims.Subject, Issuer: issuer, Run: claims.Run, Expires: claims.ExpiresAt.Time,
		Valid: func() bool {
			k, ok := v.keys(issuer, kid)
			return ok && k.Equal(key)
		},
	}, nil
}
