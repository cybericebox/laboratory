package l7

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// A catalog test-deploy token has no event and no team. In per-user mode it
// passes the same policy check as any token (the backend allows the author's
// client for the test lab) and reaches the lab, but nothing is counted.
func TestHandler_TestDeployTokenIsAuthorizedByPolicyAndNeverCounted(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer backend.Close()

	rules := []laboratoryv1alpha1.LabGroupAccessRule{{
		Action: laboratoryv1alpha1.LabGroupAccessAllow, ClientNames: []string{ClientName("author")}, LabNames: []string{"lab"},
	}}
	meter := NewMeter("boot", time.Now())
	h := NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backend.URL, nil }).
		WithTokenMode(ModePerUser).
		WithAccounting(meter, func(task, groupID string) (string, bool) { return "lab", true }).
		WithAuthorizer(func(group, subject, lab string) bool {
			return PolicyAllows(rules, ClientName(subject), lab, true)
		})

	call := func(sub, test string) int {
		tok := signToken(t, priv, jwtClaims{GroupID: "t-1", TestDeploy: test, RegisteredClaims: jwt.RegisteredClaims{Subject: sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
		req := httptest.NewRequest("GET", "http://web-abc123.challenges.example.com/", nil)
		req.Host = "web-abc123.challenges.example.com"
		req.AddCookie(&http.Cookie{Name: "challenge", Value: tok})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("author", "deploy-1"); code != 200 {
		t.Fatalf("the author of the test deploy: %d", code)
	}
	if code := call("someone-else", "deploy-1"); code != http.StatusForbidden {
		t.Fatalf("a user without an allow rule: %d", code)
	}
	if rows, _ := meter.Ledger(laboratoryv1alpha1.LabGroupNamespace("t-1")); len(rows) != 0 {
		t.Fatalf("test traffic must not be counted: %+v", rows)
	}
	if meter.Legacy() != 0 {
		t.Fatalf("test traffic is not legacy traffic either: %d", meter.Legacy())
	}
	// The same subject with an event-style token (no test claim) is counted.
	if code := call("author", ""); code != 200 {
		t.Fatalf("regular token: %d", code)
	}
	if rows, _ := meter.Ledger(laboratoryv1alpha1.LabGroupNamespace("t-1")); len(rows) != 1 {
		t.Fatalf("a regular token is counted: %+v", rows)
	}
}
