// Package web embeds the server-rendered pages and static UI assets (DESIGN.md §2.3).
package web

import "embed"

// Assets are compiled into the server; deployment needs no separate web directory.
//
//go:embed *.html *.css *.js
var Assets embed.FS
