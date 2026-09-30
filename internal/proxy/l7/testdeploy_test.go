package l7

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// A catalog test-deploy token is {group, client} like any other: the proxy
// authorizes it by the group policy and reports it as (group, client, lab).
// Telling test groups from event groups is the platform's job.
func TestHandler_TestDeployTokenIsAuthorizedByPolicyAndReportedByClient(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer backend.Close()

	rules := []laboratoryv1alpha1.LabGroupAccessRule{{
		Action: laboratoryv1alpha1.LabGroupAccessAllow, ClientNames: []string{"p-author"}, LabNames: []string{"lab"},
	}}
	meter := NewMeter("boot", time.Now())
	h := NewHandler(func() ed25519.PublicKey { return pub }, testSecret, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backend.URL, nil }).
		WithTokenMode(ModePerUser).
		WithAccounting(meter, func(task, groupID string) (string, bool) { return "lab", true }).
		WithAuthorizer(func(group, client, lab string) bool { return PolicyAllows(rules, client, lab, true) })

	call := func(client string) int {
		tok := signCookie(t, jwtClaims{GroupID: "t-1", Client: client, Version: 3, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
		req := httptest.NewRequest("GET", "http://web-abc123.challenges.example.com/", nil)
		req.Host = "web-abc123.challenges.example.com"
		req.AddCookie(&http.Cookie{Name: "challenge", Value: tok})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("p-author"); code != 200 {
		t.Fatalf("the author's client: %d", code)
	}
	if code := call("p-someone-else"); code != http.StatusForbidden {
		t.Fatalf("a client without an allow rule: %d", code)
	}
	rows, _ := meter.Ledger(laboratoryv1alpha1.LabGroupNamespace("t-1"))
	if len(rows) != 1 || rows[0].Subject != "p-author" || rows[0].Lab != "lab" || rows[0].Attempts != 1 {
		t.Fatalf("reported by (group, client, lab): %+v", rows)
	}
}
