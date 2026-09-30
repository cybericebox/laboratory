package l7

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

//go:embed assets/crest.webp
var crestWebP []byte

var crestDataURI = "data:image/webp;base64," + base64.StdEncoding.EncodeToString(crestWebP)

type cardText struct{ lang, title, hint string }

var (
	cardUK = cardText{"uk", "Сесія завершилася.", "Відкрийте лабораторію ще раз за посиланням із завдання."}
	cardEN = cardText{"en", "The session has ended.", "Open the lab again from the link in the task."}
)

// pickCard chooses the first language of Accept-Language: Ukrainian by default,
// English when the browser asks for it first.
func pickCard(acceptLanguage string) (first, second cardText) {
	for _, part := range strings.Split(acceptLanguage, ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case strings.HasPrefix(tag, "uk"):
			return cardUK, cardEN
		case strings.HasPrefix(tag, "en"):
			return cardEN, cardUK
		}
	}
	return cardUK, cardEN
}

// expired answers a request without a valid session with a static card: the
// proxy does not know which platform URL to send the visitor to.
func (h *Handler) expired(w http.ResponseWriter, r *http.Request) {
	first, second := pickCard(r.Header.Get("Accept-Language"))
	page := fmt.Sprintf(`<!doctype html>
<html lang="%s"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer"><meta name="robots" content="noindex">
<title>%s</title>
<style>
:root{color-scheme:light dark;--bg:#f7fafc;--fg:#0b1f3a;--muted:#4a5b70;--card:#fff;--line:#d9e2ec}
@media (prefers-color-scheme:dark){:root{--bg:#0b1220;--fg:#e8eef7;--muted:#9fb0c6;--card:#111a2b;--line:#22314a}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:16px;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{width:100%%;max-width:420px;text-align:center;background:var(--card);border:1px solid var(--line);border-radius:16px;padding:32px 24px}
img{width:64px;height:64px}h1{font-size:20px;margin:16px 0 8px}p{margin:0;color:var(--muted)}
.alt{margin-top:20px;padding-top:16px;border-top:1px solid var(--line);font-size:14px}.alt strong{display:block;color:var(--fg);font-weight:600}
</style></head><body><main>
<img src="%s" alt="" width="64" height="64">
<h1>%s</h1><p>%s</p>
<div class="alt" lang="%s"><strong>%s</strong>%s</div>
</main></body></html>`,
		first.lang, first.title, crestDataURI, first.title, first.hint,
		second.lang, second.title, second.hint)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(page))
}
