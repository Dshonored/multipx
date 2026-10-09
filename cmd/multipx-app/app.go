package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/quic-go/quic-go"

	"multipx/internal/client"
	"multipx/internal/daemon"
	"multipx/internal/proto"
)

// App is the Wails-bound backend: every exported method here is callable
// directly from the frontend. It talks to multipxd exactly like the TUI
// and CLI clients do, over the same QUIC protocol — this app is just
// another client, not a different implementation of session management.
type App struct {
	ctx context.Context

	mu      sync.Mutex
	c       *client.Client
	addr    string
	connErr error
	streams map[string]*quic.Stream // sessionID -> live attach stream
}

func NewApp() *App {
	return &App{streams: make(map[string]*quic.Stream)}
}

// DefaultAddr is the loopback address multipxd listens on out of the box,
// used as the fallback when the frontend has no remembered address yet
// (first launch, or local storage cleared).
func (a *App) DefaultAddr() string {
	return daemon.DefaultAddr
}

// LocalToken returns this machine's own auth token — the same file a
// local multipxd generates/reads via EnsureToken. The app and a daemon
// running under the same user account converge on this automatically, so
// connecting to the local daemon needs no manual token entry. A remote
// daemon's token has no such shortcut: fetch it over SSH with
// "multipxd -print-token" and enter it in Settings when adding that host.
func (a *App) LocalToken() (string, error) {
	return daemon.EnsureToken()
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// No auto-dial here: the frontend always calls ConnectTo explicitly,
	// with either the last address it remembered (localStorage) or
	// DefaultAddr — one connection path instead of two.
}

// ConnectTo dials a daemon at addr — local or remote — authenticating
// with token, and replacing any existing connection. Every live attach
// stream from the previous connection is closed first, since those
// streams belong to a QUIC connection that's about to go away.
//
// NOTE: remote hosts inherit the same TLS posture as loopback today
// (InsecureSkipVerify — see transport.ServerTLSConfig's TODO on real
// trust). The token stops any LAN device from opening sessions on your
// daemon, but it doesn't authenticate the daemon's identity to the
// client. Fine on a trusted LAN, not something to point at the open
// internet yet.
func (a *App) ConnectTo(addr, token string) error {
	c, err := client.Dial(a.ctx, addr, token)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.connErr = err
		return err
	}
	for _, s := range a.streams {
		_ = s.Close()
	}
	a.streams = make(map[string]*quic.Stream)
	if a.c != nil {
		_ = a.c.Close()
	}
	a.c, a.addr, a.connErr = c, addr, nil
	return nil
}

// CurrentAddr returns the address of the daemon this App is currently
// connected to, or empty if not yet connected.
func (a *App) CurrentAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addr
}

// GetUsage returns the connected daemon host's current CPU/memory usage.
func (a *App) GetUsage() (proto.UsageMsg, error) {
	c, err := a.client()
	if err != nil {
		return proto.UsageMsg{}, err
	}
	return c.GetUsage()
}

func (a *App) client() (*client.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.c == nil {
		if a.connErr == nil {
			a.connErr = fmt.Errorf("not connected to multipxd")
		}
		return nil, a.connErr
	}
	return a.c, nil
}

// ConnectionStatus reports whether the daemon connection is up, so the
// frontend can show a real error instead of a silently empty session list.
func (a *App) ConnectionStatus() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.c != nil {
		return "connected"
	}
	if a.connErr != nil {
		return "error: " + a.connErr.Error()
	}
	return "connecting"
}

func (a *App) ListSessions() ([]proto.SessionInfo, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	return c.List()
}

func (a *App) CreateSession(name string, cols, rows int) (proto.SessionInfo, error) {
	c, err := a.client()
	if err != nil {
		return proto.SessionInfo{}, err
	}
	return c.Create(name, "", cols, rows)
}

func (a *App) KillSession(id string) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	a.Detach(id)
	return c.Kill(id)
}

func (a *App) RenameSession(id, name string) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	return c.Rename(id, name)
}

func (a *App) ResizeSession(id string, cols, rows int) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	return c.Resize(id, cols, rows)
}

// Attach opens a live stream for id and starts forwarding its output to the
// frontend as "session:output:<id>" events (base64, since PTY bytes are not
// guaranteed valid UTF-8 and the JS bridge is JSON/text). Replayed
// scrollback arrives as the first such event, same as every other client.
func (a *App) Attach(id string, cols, rows int) error {
	c, err := a.client()
	if err != nil {
		return err
	}

	a.mu.Lock()
	if old, ok := a.streams[id]; ok {
		_ = old.Close()
		delete(a.streams, id)
	}
	a.mu.Unlock()

	stream, _, err := c.AttachStream(a.ctx, id, cols, rows)
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.streams[id] = stream
	a.mu.Unlock()

	go a.readPump(id, stream)
	return nil
}

// readPump forwards stream output to the frontend. Naively emitting one
// Wails event per Read would mean one JSON-marshaled IPC round-trip into
// the webview per network packet — fine for a local pipe's big reads, but
// over a network connection the OS delivers many more, smaller reads, and
// that per-chunk IPC overhead becomes the actual frame-rate ceiling (not
// the QUIC protocol, which stays well under a millisecond on a LAN). So
// reads are coalesced into a pending buffer and flushed on a short ticker
// instead of immediately: bursty output collapses into a few emits per
// frame, while the 4ms tick keeps worst-case added latency far below a
// single frame even at 120fps (~8.3ms).
func (a *App) readPump(id string, stream *quic.Stream) {
	chunks := make(chan []byte, 256)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				chunks <- chunk
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()

	var pending []byte
	flush := func() {
		if len(pending) == 0 {
			return
		}
		wailsruntime.EventsEmit(a.ctx, "session:output:"+id, base64.StdEncoding.EncodeToString(pending))
		pending = pending[:0]
	}

	ticker := time.NewTicker(4 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				flush()
				wailsruntime.EventsEmit(a.ctx, "session:closed:"+id, nil)
				return
			}
			pending = append(pending, chunk...)
			if len(pending) >= 512*1024 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Input forwards base64-encoded keystrokes/paste data to the session's PTY.
func (a *App) Input(id string, dataB64 string) error {
	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return err
	}
	a.mu.Lock()
	stream, ok := a.streams[id]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("not attached to %s", id)
	}
	_, err = stream.Write(data)
	return err
}

// Detach closes the frontend's view of a session's stream. The session
// itself keeps running on the daemon.
func (a *App) Detach(id string) {
	a.mu.Lock()
	stream, ok := a.streams[id]
	delete(a.streams, id)
	a.mu.Unlock()
	if ok {
		_ = stream.Close()
	}
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.streams {
		_ = s.Close()
	}
	if a.c != nil {
		_ = a.c.Close()
	}
}
