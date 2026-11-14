package l7

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultGRPCTokenMaxLifetime is the hard cap of exp - iat of a gRPC access token. A caller that drives a device (the brain, for an AI
// opponent or the grader) holds the token for its run, so it lives longer than a handoff link; it never lives longer than this.
const DefaultGRPCTokenMaxLifetime = 24 * time.Hour

// grpcClaims is the bearer token of a non-browser client of the gRPC access of a device (EDA web-direct, programmatic level). It is
// signed like a handoff link (EdDSA, iss = the tenant, header kid = the access key id, aud = HandoffAudience), and says: sub = the
// LabGroupClient the calls act as, group_id = the LabGroup, host = the access host of the device (<device>-<code>g) it is for, run = the
// run it belongs to, and iat, nbf, exp: exp ends every call and stream made with it.
type grpcClaims struct {
	GroupID string `json:"group_id"`
	Host    string `json:"host"`
	// Run is the run (of an opponent or of the grader) the token was issued for; the proxy requires it and does not interpret it.
	Run string `json:"run"`
	jwt.RegisteredClaims
}

// verifyGRPCToken checks the bearer token of a gRPC call exactly as verifyHandoff checks a link (signature against the key of (iss, kid),
// aud, iat/nbf/exp, host, the group belongs to the issuer), with the lifetime cap of the kind and a run. It returns the claims and the id
// of the key that signed the token.
func verifyGRPCToken(raw string, keys KeyLookup, groupTenant GroupTenant, host string, now func() time.Time, maxLifetime time.Duration) (grpcClaims, string, error) {
	var claims grpcClaims
	token, err := jwt.ParseWithClaims(
		raw, &claims, accessKeyFunc(keys),
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithTimeFunc(now), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithAudience(HandoffAudience), jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return grpcClaims{}, "", fmt.Errorf("invalid token: %w", err)
	}
	if !token.Valid {
		return grpcClaims{}, "", fmt.Errorf("invalid token")
	}
	if claims.GroupID == "" || claims.Subject == "" || claims.Host == "" || claims.Run == "" {
		return grpcClaims{}, "", fmt.Errorf("incomplete token")
	}
	if claims.IssuedAt == nil || claims.NotBefore == nil || claims.ExpiresAt == nil {
		return grpcClaims{}, "", fmt.Errorf("token needs iat, nbf and exp")
	}
	if maxLifetime <= 0 {
		maxLifetime = DefaultGRPCTokenMaxLifetime
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt.Time) > maxLifetime {
		return grpcClaims{}, "", fmt.Errorf("token lives too long")
	}
	if claims.Host != host {
		return grpcClaims{}, "", fmt.Errorf("token is for another host")
	}
	if groupTenant == nil {
		return grpcClaims{}, "", fmt.Errorf("no way to tell the tenant of a group")
	}
	if owner, ok := groupTenant(claims.GroupID); !ok || owner != claims.Issuer {
		return grpcClaims{}, "", fmt.Errorf("the group does not belong to the issuer")
	}
	kid, _ := token.Header["kid"].(string)
	return claims, kid, nil
}

// bearerToken is the token of an "Authorization: Bearer <token>" header.
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
