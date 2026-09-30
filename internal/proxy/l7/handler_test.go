package l7

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func makeToken(t *testing.T, priv *rsa.PrivateKey, groupID string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(
		jwt.SigningMethodRS256, jwtClaims{
			GroupID: groupID,
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		},
	).SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}

func TestHandler_StripsCookie(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	var gotCookieHeader string
	backend := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotCookieHeader = r.Header.Get("Cookie")
				w.WriteHeader(200)
			},
		),
	)
	defer backend.Close()

	resolver := func(task, groupID string) (string, error) {
		return backend.URL, nil
	}

	h := NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, "challenges.example.com", "challenge", resolver)

	req := httptest.NewRequest("GET", "http://mytask.challenges.example.com/path", nil)
	req.Host = "mytask.challenges.example.com"
	req.AddCookie(&http.Cookie{Name: "challenge", Value: makeToken(t, priv, "grp1")})
	req.AddCookie(&http.Cookie{Name: "other", Value: "keep"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if strings.Contains(gotCookieHeader, "challenge=") {
		t.Fatalf("challenge cookie was NOT stripped: %s", gotCookieHeader)
	}
	if !strings.Contains(gotCookieHeader, "other=keep") {
		t.Fatalf("other cookie was stripped: %s", gotCookieHeader)
	}
}

func TestHandler_InvalidHost(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	h := NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, "challenges.example.com", "challenge", nil)

	req := httptest.NewRequest("GET", "http://evil.com/", nil)
	req.Host = "evil.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_CountsPerUserRequestsAndBytes(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("hello lab"))
	}))
	defer backend.Close()

	meter := NewMeter("boot-1", time.Now())
	h := NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backend.URL, nil }).
		WithTokenMode(ModeMixed).
		WithAccounting(meter, func(task, groupID string) (string, bool) { return "c-1", true })

	send := func(token, body string) int {
		req := httptest.NewRequest("POST", "http://web-abc123.challenges.example.com/secret/path?q=1", strings.NewReader(body))
		req.Host = "web-abc123.challenges.example.com"
		req.AddCookie(&http.Cookie{Name: "challenge", Value: token})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	user := signToken(t, priv, jwtClaims{GroupID: "g1", RegisteredClaims: jwt.RegisteredClaims{Subject: "user-1", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
	if code := send(user, "12345"); code != 200 {
		t.Fatalf("code = %d", code)
	}
	if code := send(user, ""); code != 200 {
		t.Fatalf("code = %d", code)
	}
	legacy := signToken(t, priv, jwtClaims{GroupID: "g1", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
	if code := send(legacy, ""); code != 200 {
		t.Fatalf("legacy code = %d", code)
	}

	rows, truncated := meter.Ledger(laboratoryv1alpha1.LabGroupNamespace("g1"))
	if truncated || len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	row := rows[0]
	if row.Subject != "user-1" || row.Lab != "c-1" || row.Attempts != 2 || row.BytesIn != 18 || row.BytesOut != 5 || row.RespondedMs == 0 {
		t.Fatalf("row = %+v", row)
	}
	if meter.Legacy() != 1 {
		t.Fatalf("legacy requests = %d", meter.Legacy())
	}
}

func TestHandler_UpstreamFailureIsNotAResponse(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()

	meter := NewMeter("boot-1", time.Now())
	h := NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return url, nil }).
		WithTokenMode(ModePerUser).
		WithAccounting(meter, func(task, groupID string) (string, bool) { return "c-1", true })
	req := httptest.NewRequest("GET", "http://web-abc123.challenges.example.com/", nil)
	req.Host = "web-abc123.challenges.example.com"
	req.AddCookie(&http.Cookie{Name: "challenge", Value: signToken(t, priv, jwtClaims{GroupID: "g1", RegisteredClaims: jwt.RegisteredClaims{Subject: "user-1", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d", rec.Code)
	}
	rows, _ := meter.Ledger(laboratoryv1alpha1.LabGroupNamespace("g1"))
	if len(rows) != 1 || rows[0].Attempts != 1 || rows[0].RespondedMs != 0 {
		t.Fatalf("an attempt the lab never answered must not count as reached: %+v", rows)
	}
}

func TestHandler_TokenModesAndAuthorizer(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer backend.Close()
	token := func(sub string) string {
		return signToken(t, priv, jwtClaims{GroupID: "g1", RegisteredClaims: jwt.RegisteredClaims{Subject: sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
	}
	call := func(h *Handler, tok string) int {
		req := httptest.NewRequest("GET", "http://web-abc123.challenges.example.com/", nil)
		req.Host = "web-abc123.challenges.example.com"
		req.AddCookie(&http.Cookie{Name: "challenge", Value: tok})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	build := func(mode TokenMode, authorize Authorizer) *Handler {
		h := NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, "challenges.example.com", "challenge",
			func(task, groupID string) (string, error) { return backend.URL, nil }).WithTokenMode(mode)
		if authorize != nil {
			h.WithAuthorizer(authorize).WithAccounting(NewMeter("b", time.Now()), func(task, groupID string) (string, bool) { return "c-1", true })
		}
		return h
	}
	if code := call(build(ModePerUser, nil), token("")); code != http.StatusUnauthorized {
		t.Fatalf("per-user without a subject: %d", code)
	}
	if code := call(build(ModeMixed, nil), token("")); code != 200 {
		t.Fatalf("mixed without a subject: %d", code)
	}
	deny := func(group, subject, lab string) bool { return subject != "banned" }
	if code := call(build(ModePerUser, deny), token("banned")); code != http.StatusForbidden {
		t.Fatalf("revoked participant: %d", code)
	}
	if code := call(build(ModePerUser, deny), token("user-1")); code != 200 {
		t.Fatalf("allowed participant: %d", code)
	}
}

// A valid, unexpired token of a member who is no longer in the group policy is
// refused in per-user mode: revocation is done by the policy, not by the token.
func TestHandler_PerUserRefusesAValidTokenOfARemovedMember(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer backend.Close()
	rules := []laboratoryv1alpha1.LabGroupAccessRule{{
		Action: laboratoryv1alpha1.LabGroupAccessAllow, ClientNames: []string{ClientName("member")}, LabNames: []string{"c-1"},
	}}
	authorize := func(group, subject, lab string) bool { return PolicyAllows(rules, ClientName(subject), lab, true) }
	h := NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backend.URL, nil }).
		WithTokenMode(ModePerUser).
		WithAccounting(NewMeter("b", time.Now()), func(task, groupID string) (string, bool) { return "c-1", true }).
		WithAuthorizer(authorize)
	call := func(sub string) int {
		tok := signToken(t, priv, jwtClaims{GroupID: "g1", RegisteredClaims: jwt.RegisteredClaims{Subject: sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(72 * time.Hour))}})
		req := httptest.NewRequest("GET", "http://web-abc123.challenges.example.com/", nil)
		req.Host = "web-abc123.challenges.example.com"
		req.AddCookie(&http.Cookie{Name: "challenge", Value: tok})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("member"); code != 200 {
		t.Fatalf("member in the policy: %d", code)
	}
	if code := call("removed"); code != http.StatusForbidden {
		t.Fatalf("removed member with a valid token must be refused: %d", code)
	}
}
