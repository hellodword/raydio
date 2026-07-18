package web

import "embed"

// FS contains the browser player.
//
//go:embed index.html config.json app.js styles.css
var FS embed.FS
