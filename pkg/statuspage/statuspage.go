// Package statuspage is the one look of the small static pages the laboratory serves to a browser: the VPN
// check page inside the tunnel and the proxy's error cards. Style, theme switch and its script live here once.
package statuspage

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
)

//go:embed style.css
var style string

//go:embed theme.js
var script string

//go:embed favicon.png
var faviconPNG []byte

//go:embed theme.html
var themeHTML string

// Style is the shared CSS (inline, no external hosts).
func Style() template.CSS { return template.CSS(style) }

// Script is the theme switch script.
func Script() template.JS { return template.JS(script) }

// Theme is the light/dark/system switch with the labels in the page language ("en", anything else is Ukrainian).
func Theme(lang string) template.HTML {
	if lang == "en" {
		return template.HTML(fmt.Sprintf(themeHTML, "Page theme", "Light", "Dark", "System"))
	}
	return template.HTML(fmt.Sprintf(themeHTML, "Тема сторінки", "Світла", "Темна", "Системна"))
}

// Favicon is the platform icon (32x32 PNG) as a data URI for <link rel="icon">:
// the pages cannot load it from a platform host.
func Favicon() template.URL {
	return template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(faviconPNG))
}

// State is the kind of a card: it picks the icon colour and glyph.
type State string

const (
	// StateOK is a success (VPN connected): green check.
	StateOK State = "ok"
	// StateWait is a card that asks the visitor to act or wait (session ended, too many requests,
	// the task not ready): amber clock.
	StateWait State = "warn"
	// StateUnavailable is a card for something that is not there or broke (task not available, bad host,
	// internal error): red slashed circle.
	StateUnavailable State = "danger"
)

var glyphs = map[State]string{
	StateOK:          `<path d="M5 12.5l4.2 4.2L19 7"/>`,
	StateWait:        `<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>`,
	StateUnavailable: `<circle cx="12" cy="12" r="8.5"/><path d="M6 18L18 6"/>`,
}

// Icon is the state icon (56px circle, 28px glyph); the title is the caller's and the brand Rule goes under it.
func Icon(s State) template.HTML {
	g, ok := glyphs[s]
	if !ok {
		s, g = StateUnavailable, glyphs[StateUnavailable]
	}
	return template.HTML(`<span class="ic ic-` + string(s) + `">` +
		`<svg viewBox="0 0 24 24" aria-hidden="true">` + g + `</svg></span>`)
}

// Rule is the short brand-colour line under the title.
const Rule template.HTML = `<span class="rule" aria-hidden="true"></span>`
