package l7

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// The terminal library and the page code are served by the proxy itself from files embedded in the binary: the page loads nothing from
// another host (no CDN), which the Content-Security-Policy of the console host also enforces.
//
//go:embed terminalassets
var terminalFS embed.FS

// terminalAssetTypes is the list of files that may be served, with their types. Nothing else of the embedded tree is reachable.
var terminalAssetTypes = map[string]string{
	"xterm.js":           "text/javascript; charset=utf-8",
	"addon-fit.js":       "text/javascript; charset=utf-8",
	"addon-web-links.js": "text/javascript; charset=utf-8",
	"terminal.js":        "text/javascript; charset=utf-8",
	"xterm.css":          "text/css; charset=utf-8",
	"terminal.css":       "text/css; charset=utf-8",
}

func serveTerminalAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	typ, ok := terminalAssetTypes[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := terminalFS.ReadFile("terminalassets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(body)
}

// terminalText is the text of the page in one language.
type terminalText struct {
	Lang       string            `json:"-"`
	Title      string            `json:"-"`
	Upload     string            `json:"-"`
	Download   string            `json:"-"`
	UploadDir  string            `json:"-"`
	DownPath   string            `json:"-"`
	Again      string            `json:"-"`
	Connecting string            `json:"connecting"`
	Connected  string            `json:"connected"`
	Ended      string            `json:"ended"`
	Reasons    map[string]string `json:"reasons"`
	Files      map[string]string `json:"files"`
}

var terminalUK = terminalText{
	Lang: "uk", Title: "Термінал", Upload: "Завантажити на пристрій", Download: "Скачати з пристрою",
	UploadDir: "Каталог на пристрої", DownPath: "Шлях до файлу на пристрої", Again: "Підключитися знову",
	Connecting: "З’єднуємо…", Connected: "Підключено", Ended: "Сеанс завершено",
	Reasons: map[string]string{
		"exit":               "Оболонку завершено.",
		"client":             "З’єднання закрито.",
		"idle":               "Сеанс закрито через неактивність.",
		"lifetime":           "Сеанс досяг максимальної тривалості.",
		"access-lost":        "Доступ до лабораторії втрачено.",
		wire.CodeLimit:       "Забагато відкритих терміналів.",
		wire.CodeDisabled:    "Для цього пристрою термінал вимкнено.",
		wire.CodeNotFound:    "Пристрій зараз недоступний.",
		wire.CodeUnavailable: "Термінал тимчасово недоступний. Спробуйте за хвилину.",
		"stalled":            "Сеанс закрито: сторінка не встигає за виводом.",
		"shutdown":           "Сервіс перезапускається.",
		"error":              "Сталася помилка з’єднання.",
		wire.CodeBadRequest:  "Сталася помилка з’єднання.",
	},
	Files: map[string]string{
		"uploading":          "Завантажуємо…",
		"uploaded":           "Файл завантажено:",
		"downloading":        "Скачуємо…",
		"downloaded":         "Файл скачано.",
		wire.CodeNotFound:    "Файл не знайдено.",
		wire.CodeDenied:      "Немає прав на цей файл.",
		wire.CodeNotRegular:  "Це не звичайний файл.",
		wire.CodeTooBig:      "Файл завеликий або вичерпано ліміт сеансу.",
		wire.CodeBusy:        "Зачекайте, поки завершиться попередня передача.",
		wire.CodeLimit:       "Забагато передач одночасно.",
		wire.CodeBadRequest:  "Некоректний шлях.",
		wire.CodeIncomplete:  "Передачу перервано.",
		wire.CodeUnavailable: "Передача файлів зараз недоступна.",
	},
}

var terminalEN = terminalText{
	Lang: "en", Title: "Terminal", Upload: "Upload to the device", Download: "Download from the device",
	UploadDir: "Directory on the device", DownPath: "Path of the file on the device", Again: "Reconnect",
	Connecting: "Connecting…", Connected: "Connected", Ended: "Session ended",
	Reasons: map[string]string{
		"exit":               "The shell has exited.",
		"client":             "The connection was closed.",
		"idle":               "The session was closed because it was idle.",
		"lifetime":           "The session reached its maximum length.",
		"access-lost":        "Access to the lab was lost.",
		wire.CodeLimit:       "Too many terminals are open.",
		wire.CodeDisabled:    "The terminal is turned off for this device.",
		wire.CodeNotFound:    "The device is not available right now.",
		wire.CodeUnavailable: "The terminal is temporarily unavailable. Try again in a minute.",
		"stalled":            "The session was closed: the page cannot keep up with the output.",
		"shutdown":           "The service is restarting.",
		"error":              "A connection error occurred.",
		wire.CodeBadRequest:  "A connection error occurred.",
	},
	Files: map[string]string{
		"uploading":          "Uploading…",
		"uploaded":           "File uploaded:",
		"downloading":        "Downloading…",
		"downloaded":         "File downloaded.",
		wire.CodeNotFound:    "File not found.",
		wire.CodeDenied:      "No permission for this file.",
		wire.CodeNotRegular:  "This is not a regular file.",
		wire.CodeTooBig:      "The file is too large or the session limit is used up.",
		wire.CodeBusy:        "Wait for the previous transfer to finish.",
		wire.CodeLimit:       "Too many transfers at once.",
		wire.CodeBadRequest:  "Invalid path.",
		wire.CodeIncomplete:  "The transfer was interrupted.",
		wire.CodeUnavailable: "File transfer is not available right now.",
	},
}

// pickTerminalText follows the language policy of the lab pages: the first language of Accept-Language, Ukrainian by default.
func pickTerminalText(acceptLanguage string) terminalText {
	card := pickCard(acceptLanguage, page{uk: cardText{lang: "uk"}, en: cardText{lang: "en"}})
	if card.lang == "en" {
		return terminalEN
	}
	return terminalUK
}

var terminalTmpl = template.Must(template.New("terminal").Parse(`<!doctype html>
<html lang="{{.T.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer"><meta name="robots" content="noindex">
<title>{{.T.Title}}</title>
<link rel="stylesheet" href="/assets/xterm.css">
<link rel="stylesheet" href="/assets/terminal.css">
</head>
<body>
<div id="app">
  <header>
    <h1>{{.T.Title}}</h1>
    <span id="target">{{.Lab}} / {{.Device}}</span>
    <div class="files">
      <input id="updir" type="text" value="/tmp" aria-label="{{.T.UploadDir}}" title="{{.T.UploadDir}}" maxlength="1024" spellcheck="false" autocomplete="off">
      <button id="upload" type="button" disabled>{{.T.Upload}}</button>
      <input id="upfile" type="file">
      <input id="downpath" type="text" placeholder="/path/file" aria-label="{{.T.DownPath}}" title="{{.T.DownPath}}" maxlength="1024" spellcheck="false" autocomplete="off">
      <button id="download" type="button" disabled>{{.T.Download}}</button>
    </div>
    <span id="status"></span>
  </header>
  <div id="note" role="status"></div>
  <div id="term"></div>
</div>
<div id="over" role="alertdialog" aria-labelledby="over-text"><div><p id="over-text"></p><button id="again" type="button">{{.T.Again}}</button></div></div>
<script type="application/json" id="i18n">{{.I18N}}</script>
<script src="/assets/xterm.js"></script>
<script src="/assets/addon-fit.js"></script>
<script src="/assets/addon-web-links.js"></script>
<script src="/assets/terminal.js"></script>
</body>
</html>
`))

func serveTerminalPage(w http.ResponseWriter, r *http.Request, tg terminalTarget) {
	t := pickTerminalText(r.Header.Get("Accept-Language"))
	raw, _ := json.Marshal(t)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = terminalTmpl.Execute(w, struct {
		T      terminalText
		Lab    string
		Device string
		I18N   template.JS
	}{t, tg.lab, tg.device, template.JS(raw)})
}
