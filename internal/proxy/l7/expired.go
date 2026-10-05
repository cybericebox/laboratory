package l7

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/cybericebox/laboratory/pkg/statuspage"
)

type cardText struct{ lang, title, hint string }

// page is one error card: its text in both languages (a visitor sees one). The status code is the caller's.
type page struct {
	uk, en cardText
	state  statuspage.State
}

var (
	pageExpired = page{
		cardText{"uk", "Сесію завершено", "Щоб продовжити, відкрийте лабораторію за посиланням у завданні."},
		cardText{"en", "Session ended", "To continue, open the lab from the link in the task."},
		statuspage.StateWait,
	}
	// pageGone is the same for an unknown host, a removed lab and a refused client, so the text does not tell which.
	pageGone = page{
		cardText{"uk", "Такого завдання зараз немає", "Можливо, лабораторію вже вимкнено або посилання застаріло."},
		cardText{"en", "This task is not available right now", "The lab may have been turned off or the link is out of date."},
		statuspage.StateUnavailable,
	}
	pageUpstream = page{
		cardText{"uk", "Завдання поки недоступне", "Сервіс завдання ще запускається або недоступний. Спробуйте за хвилину."},
		cardText{"en", "The task is not available yet", "The task service is still starting or unavailable. Try again in a minute."},
		statuspage.StateWait,
	}
	pageBusy = page{
		cardText{"uk", "Забагато запитів", "Спробуйте ще раз за кілька секунд."},
		cardText{"en", "Too many requests", "Try again in a few seconds."},
		statuspage.StateWait,
	}
	pageFailed = page{
		cardText{"uk", "Щось пішло не так", "Спробуйте ще раз за хвилину."},
		cardText{"en", "Something went wrong", "Try again in a minute."},
		statuspage.StateUnavailable,
	}
)

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
  <meta name="referrer" content="no-referrer"><meta name="robots" content="noindex">
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
		card.lang, statuspage.Favicon(), card.title, statuspage.Style(), p.state, statuspage.Icon(p.state), card.title, statuspage.Rule, card.hint,
		statuspage.Theme(card.lang), statuspage.Script())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
