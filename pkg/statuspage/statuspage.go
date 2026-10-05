// Package statuspage is the one look of the small static pages the laboratory serves to a browser: the VPN
// check page inside the tunnel and the proxy's error cards. Style, theme switch and its script live here once.
package statuspage

import (
	_ "embed"
	"fmt"
	"html/template"
)

//go:embed style.css
var style string

//go:embed theme.js
var script string

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
