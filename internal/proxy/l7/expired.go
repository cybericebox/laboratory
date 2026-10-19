package l7

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"

	"github.com/cybericebox/laboratory/pkg/statuspage"
)

type cardText struct{ lang, title, hint string }

// page is one error card: its text in both languages (a visitor sees one). The status code is the caller's.
type page struct {
	uk, en cardText
	state  statuspage.State
	// retry is the seconds after which a browser asks again by itself (0: it does not). Only a GET or HEAD is repeated.
	retry int
}

var (
	pageExpired = page{
		cardText{"uk", "Сесію завершено", "Щоб продовжити, відкрийте лабораторію за посиланням у завданні."},
		cardText{"en", "Session ended", "To continue, open the lab from the link in the task."},
		statuspage.StateWait,
		0,
	}
	// pageGone is the same for an unknown host, a removed lab and a refused client, so the text does not tell which.
	pageGone = page{
		cardText{"uk", "Такого завдання зараз немає", "Можливо, лабораторію вже вимкнено або посилання застаріло."},
		cardText{"en", "This task is not available right now", "The lab may have been turned off or the link is out of date."},
		statuspage.StateUnavailable,
		0,
	}
	pageUpstream = page{
		cardText{"uk", "Завдання поки недоступне", "Сервіс завдання ще запускається або недоступний. Спробуйте за хвилину."},
		cardText{"en", "The task is not available yet", "The task service is still starting or unavailable. Try again in a minute."},
		statuspage.StateWait,
		0,
	}
	// pageStarting is a lab that exists but whose service does not answer yet (the pod is starting or not Ready): not an error,
	// the page asks again by itself.
	pageStarting = page{
		cardText{"uk", "Лабораторія запускається…", "Це займає хвилину. Сторінка оновиться сама."},
		cardText{"en", "The lab is starting…", "This takes about a minute. The page refreshes by itself."},
		statuspage.StateLoading,
		startingRetrySeconds,
	}
	pageBusy = page{
		cardText{"uk", "Забагато запитів", "Спробуйте ще раз за кілька секунд."},
		cardText{"en", "Too many requests", "Try again in a few seconds."},
		statuspage.StateWait,
		0,
	}
	pageFailed = page{
		cardText{"uk", "Щось пішло не так", "Спробуйте ще раз за хвилину."},
		cardText{"en", "Something went wrong", "Try again in a minute."},
		statuspage.StateUnavailable,
		0,
	}
)

// startingRetrySeconds is how often the starting page asks the lab again.
const startingRetrySeconds = 4

// wantsHTML tells a browser (it asks for text/html) from a script or an API client.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

// pickCard chooses the first language of Accept-Language: Ukrainian by default,
// English when the browser asks for it first.
func pickCard(acceptLanguage string, p page) cardText {
	cardUK, cardEN := p.uk, p.en
	for _, part := range strings.Split(acceptLanguage, ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case strings.HasPrefix(tag, "uk"):
			return cardUK
		case strings.HasPrefix(tag, "en"):
			return cardEN
		}
	}
	return cardUK
}

// expired answers a request without a valid session with a static card: the
// proxy does not know which platform URL to send the visitor to.
func (h *Handler) expired(w http.ResponseWriter, r *http.Request) {
	fail(w, r, http.StatusUnauthorized, pageExpired, "unauthorized")
}

// fail answers a browser (Accept: text/html) with the styled card p and any other client with the short plain
// text, both with the same status. The text never names a host, a lab, a token or an address.
func fail(w http.ResponseWriter, r *http.Request, status int, p page, plain string) {
	repeat := p.retry > 0 && (r.Method == http.MethodGet || r.Method == http.MethodHead)
	if p.retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(p.retry))
	}
	if !wantsHTML(r) {
		http.Error(w, plain, status)
		return
	}
	card := pickCard(r.Header.Get("Accept-Language"), p)
	body := fmt.Sprintf(`<!doctype html>
<html lang="%s">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta name="referrer" content="no-referrer"><meta name="robots" content="noindex">%s
  <link rel="icon" type="image/png" href="%s">
  <title>%s</title>
  <style>
%s
  </style>
</head>
<body>
  <div class="frame">
    <main class="state-%s">
      %s
      <h1>%s</h1>
      %s
      <p class="intro">%s</p>
    </main>
    <footer>
%s
    </footer>
  </div>
  <script>
%s
  </script>
</body>
</html>`,
		card.lang, refresh(repeat, p.retry), statuspage.Favicon(), card.title, statuspage.Style(), p.state, statuspage.Icon(p.state), card.title, statuspage.Rule, card.hint,
		statuspage.Theme(card.lang), statuspage.Script())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// refresh is the meta tag that makes the browser load the same address again after retry seconds.
func refresh(repeat bool, retry int) string {
	if !repeat {
		return ""
	}
	return fmt.Sprintf(`<meta http-equiv="refresh" content="%d">`, retry)
}

// upstreamNotReady tells a lab that does not answer yet from one that answered wrongly: the connection to it could not be made
// (refused while the pod is not Ready, no route, a dial timeout, a name that is not resolvable yet). A client that left is no failure.
func upstreamNotReady(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var op *net.OpError
	return errors.Is(err, syscall.ECONNREFUSED) || errors.As(err, &op) && op.Op == "dial"
}

// upstreamFailed answers a request whose upstream failed: the starting page for a service that cannot be reached yet, the
// bad-gateway card for anything else.
func upstreamFailed(w http.ResponseWriter, r *http.Request, err error) {
	if upstreamNotReady(err) {
		fail(w, r, http.StatusServiceUnavailable, pageStarting, "lab is starting")
		return
	}
	fail(w, r, http.StatusBadGateway, pageUpstream, "bad gateway")
}
