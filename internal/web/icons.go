package web

import (
	"html/template"
	"sort"
	"strings"
)

// Icons are inline SVG (no icon font, no requests, allowed by the CSP). They draw with
// currentColor, so they follow the text color of whatever they sit in.
var iconPaths = map[string]string{
	"server":   `<rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/>`,
	"globe":    `<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 2.6 3.8 5.6 3.8 9s-1.3 6.4-3.8 9c-2.5-2.6-3.8-5.6-3.8-9s1.3-6.4 3.8-9z"/>`,
	"shield":   `<path d="M12 3l7.5 3v5.5c0 4.6-3.2 8.3-7.5 9.5-4.3-1.2-7.5-4.9-7.5-9.5V6z"/><path d="M9 12l2 2 4-4"/>`,
	"list":     `<path d="M9 6h11M9 12h11M9 18h11M4.5 6h.01M4.5 12h.01M4.5 18h.01"/>`,
	"logout":   `<path d="M10 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h4"/><path d="M15 8l4 4-4 4M19 12H9"/>`,
	"user":     `<circle cx="12" cy="8" r="4"/><path d="M4 21c1.5-4 4.5-6 8-6s6.5 2 8 6"/>`,
	"plus":     `<path d="M12 5v14M5 12h14"/>`,
	"logs":     `<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7 9l3 3-3 3M12 15h5"/>`,
	"restart":  `<path d="M20 12a8 8 0 1 1-2.3-5.6"/><path d="M20 4v5h-5"/>`,
	"edit":     `<path d="M4 20h4L19 9a2.8 2.8 0 0 0-4-4L4 16z"/><path d="M13.5 6.5l4 4"/>`,
	"deploy":   `<path d="M12 15V4M7.5 8.5L12 4l4.5 4.5"/><path d="M4 15v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3"/>`,
	"trash":    `<path d="M4 7h16M10 11v6M14 11v6M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4h6v3"/>`,
	"cpu":      `<rect x="6" y="6" width="12" height="12" rx="2"/><rect x="9.5" y="9.5" width="5" height="5" rx="1"/><path d="M9 2.5v3M15 2.5v3M9 18.5v3M15 18.5v3M2.5 9h3M2.5 15h3M18.5 9h3M18.5 15h3"/>`,
	"memory":   `<rect x="2.5" y="6" width="19" height="10" rx="1.5"/><path d="M6.5 16v3M10.5 16v3M13.5 16v3M17.5 16v3M7 9.5h2v3H7zM11 9.5h2v3h-2zM15 9.5h2v3h-2z"/>`,
	"swap":     `<path d="M7 4L3 8l4 4M3 8h14M17 20l4-4-4-4M21 16H7"/>`,
	"disk":     `<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 13h18M7 16.5h.01M11 16.5h.01"/>`,
	"clock":    `<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>`,
	"alert":    `<path d="M10.3 4.3L2.6 18a2 2 0 0 0 1.7 3h15.4a2 2 0 0 0 1.7-3L13.7 4.3a2 2 0 0 0-3.4 0z"/><path d="M12 9.5v4M12 17h.01"/>`,
	"check":    `<circle cx="12" cy="12" r="9"/><path d="M8.5 12.5l2.5 2.5 4.5-5"/>`,
	"info":     `<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/>`,
	"external": `<path d="M14 4h6v6M20 4l-9 9M18 14v4a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4"/>`,
	"back":     `<path d="M15 6l-6 6 6 6"/>`,
	"box":      `<path d="M12 3l8 4.5v9L12 21l-8-4.5v-9z"/><path d="M4 7.5l8 4.5 8-4.5M12 12v9"/>`,
	"pause":    `<path d="M9 5v14M15 5v14"/>`,
	"play":     `<path d="M7 5l12 7-12 7z"/>`,
	"search":   `<circle cx="11" cy="11" r="7"/><path d="M20 20l-4-4"/>`,
	"lock":     `<rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/>`,
	"download": `<path d="M12 4v11M7.5 10.5L12 15l4.5-4.5"/><path d="M4 15v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3"/>`,
	"database": `<ellipse cx="12" cy="6" rx="7.5" ry="3"/><path d="M4.5 6v12c0 1.7 3.4 3 7.5 3s7.5-1.3 7.5-3V6M4.5 12c0 1.7 3.4 3 7.5 3s7.5-1.3 7.5-3"/>`,
	"upload":   `<path d="M12 15V4M7.5 8.5L12 4l4.5 4.5"/><path d="M4 15v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3"/>`,
	"undo":     `<path d="M9 14L4 9l5-5"/><path d="M4 9h10.5a5.5 5.5 0 0 1 0 11H11"/>`,
	"stop":     `<rect x="6" y="6" width="12" height="12" rx="1.5"/>`,
}

// icon refers to a symbol of the sprite, so a table with many rows repeats a short
// reference instead of the drawing; unknown names render nothing.
func icon(name string) template.HTML {
	if _, ok := iconPaths[name]; !ok {
		return ""
	}
	return template.HTML(`<svg class="i" aria-hidden="true"><use href="#i-` + name + `"/></svg>`)
}

// sprite holds every icon once; the layout puts it at the top of each page.
var sprite = func() template.HTML {
	names := make([]string, 0, len(iconPaths))
	for n := range iconPaths {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(`<svg class="sprite" aria-hidden="true" focusable="false">`)
	for _, n := range names {
		b.WriteString(`<symbol id="i-` + n + `" viewBox="0 0 24 24">` + iconPaths[n] + `</symbol>`)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}()

// logo is the FootOps mark: a rounded square with an F.
const logo = template.HTML(`<svg class="logo" viewBox="0 0 24 24" aria-hidden="true" focusable="false"><rect x="1" y="1" width="22" height="22" rx="6.5"/><path d="M9 17V7h6.5M9 12h5"/></svg>`)
