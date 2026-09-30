// Package web embeds the server-rendered pages and static UI assets (DESIGN.md §2.3).
package web

import "embed"

// Assets are compiled into the server; deployment needs no separate web directory.
// The font is Hanken Grotesk (SIL OFL 1.1, OFL.txt), served from /static/
// (NOTES.md N-244). favicon.svg is the MusicLib symbol; brand/ holds its
// source and is not embedded: the page inlines the symbol (NOTES.md N-309).
// grain.svg is the noise tile of the sidebar's glow.
//
//go:embed *.html *.css *.js *.woff2 OFL.txt favicon.svg grain.svg
var Assets embed.FS
