// Command multipx-app is the native desktop client: the Brilliant dashboard
// design rendered as real HTML/CSS in a GPU-composited OS webview (WebView2
// on Windows, WKWebView on macOS), with xterm.js driving the actual
// terminal panes. It talks to the same multipxd daemon over the same QUIC
// protocol as the CLI and TUI — this is a frontend swap, not a rewrite.
package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:  "multipx",
		Width:  1280,
		Height: 800,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		// Alpha 0: let the Mica backdrop show through wherever the page
		// doesn't paint a solid color (the topbar/sidebar go translucent
		// in CSS); the terminal pane itself stays fully opaque for
		// legibility, same split Windows Terminal itself uses.
		BackgroundColour: &options.RGBA{R: 0x24, G: 0x27, B: 0x2a, A: 0},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			WindowIsTranslucent: true,
			BackdropType:        windows.Mica,
			Theme:               windows.Dark,
		},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}
