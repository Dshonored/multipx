// Command multipx-gui is the native desktop client, rendered with Gio: a
// real GPU surface (Direct3D/Metal/Vulkan/GL depending on OS), no webview,
// no JS terminal emulator in between. It talks to multipxd over the same
// QUIC protocol as the CLI and TUI — only the frontend differs.
package main

import (
	"context"
	"log"
	"os"

	"gioui.org/app"
	"gioui.org/unit"

	"multipx/internal/client"
	"multipx/internal/daemon"
)

func main() {
	go func() {
		w := new(app.Window)
		w.Option(app.Title("multipx"), app.Size(unit.Dp(1280), unit.Dp(800)))
		if err := run(w); err != nil {
			log.Println("multipx-gui:", err)
		}
		os.Exit(0)
	}()
	app.Main()
}

func run(w *app.Window) error {
	ctx := context.Background()
	addr := daemon.DefaultAddr
	if v := os.Getenv("MULTIPX_ADDR"); v != "" {
		addr = v
	}
	token := os.Getenv("MULTIPX_TOKEN")
	if token == "" {
		if t, err := daemon.EnsureToken(); err == nil {
			token = t
		}
	}

	c, connErr := client.Dial(ctx, addr, token)
	a := NewApp(ctx, w, c, connErr)
	defer a.Close()

	return a.EventLoop()
}
