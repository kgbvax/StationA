// Package static embeds the frontend assets served by testui.
package static

import "embed"

//go:embed *.html *.css *.js
var FS embed.FS
