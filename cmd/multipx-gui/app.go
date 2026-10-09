package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/text"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/hinshun/vt10x"
	"github.com/quic-go/quic-go"

	"multipx/internal/client"
	"multipx/internal/proto"
)

// sessionView is a session's state as seen by the GUI: its info snapshot,
// the live attach stream if focused, and a terminal emulator that keeps
// running (and keeps being redrawn) independent of whether it's the
// visible one, exactly like the TUI and native-webview clients.
type sessionView struct {
	info   proto.SessionInfo
	stream *quic.Stream
	term   vt10x.Terminal
}

// paneTag and sidebar row pointers are Gio event tags: stable identities
// hit-tested against pointer/key events declared each frame.
type paneTagT struct{}
type addRowTagT struct{}

var paneTag = &paneTagT{}
var addRowTag = &addRowTagT{}

type App struct {
	ctx    context.Context
	window *app.Window
	th     *material.Theme

	client  *client.Client
	connErr error

	mu              sync.Mutex
	sessions        []proto.SessionInfo
	views           map[string]*sessionView
	focusedID       string
	autoCreateTried bool
	status          string

	prefixArmed bool // reserved for future Ctrl-B-style commands

	lastCols, lastRows int

	// menu and rename are touched only from the Gio event-loop goroutine
	// (inside layout/event handling), never from background goroutines,
	// so unlike the fields above they need no mutex.
	menu   *menuState
	rename *renameState
}

type menuState struct {
	sessionID string
}

type renameState struct {
	sessionID string
	editor    widget.Editor
}

func NewApp(ctx context.Context, w *app.Window, c *client.Client, connErr error) *App {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))

	a := &App{
		ctx:     ctx,
		window:  w,
		th:      th,
		client:  c,
		connErr: connErr,
		views:   make(map[string]*sessionView),
	}
	go a.pollSessions()
	return a
}

func (a *App) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, v := range a.views {
		if v.stream != nil {
			_ = v.stream.Close()
		}
	}
	if a.client != nil {
		_ = a.client.Close()
	}
}

// pollSessions keeps the sidebar in sync with the daemon's session
// registry — including sessions created or killed by other clients (the
// TUI, the CLI) — the same 2s cadence the other clients use.
func (a *App) pollSessions() {
	for {
		a.refreshSessions()
		time.Sleep(2 * time.Second)
	}
}

func (a *App) refreshSessions() {
	if a.client == nil {
		return
	}
	list, err := a.client.List()

	a.mu.Lock()
	if err != nil {
		a.connErr = err
		a.mu.Unlock()
		a.window.Invalidate()
		return
	}
	a.connErr = nil
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	a.sessions = list
	for _, info := range list {
		if v, ok := a.views[info.ID]; ok {
			v.info = info
		} else {
			a.views[info.ID] = &sessionView{info: info}
		}
	}
	seen := make(map[string]bool, len(list))
	for _, info := range list {
		seen[info.ID] = true
	}
	for id, v := range a.views {
		if !seen[id] {
			if v.stream != nil {
				_ = v.stream.Close()
			}
			delete(a.views, id)
		}
	}

	needAutoCreate := len(list) == 0 && !a.autoCreateTried
	if needAutoCreate {
		a.autoCreateTried = true
	}
	needFocus := a.focusedID == "" && len(list) > 0
	focusID := ""
	if needFocus {
		focusID = list[0].ID
	}
	a.mu.Unlock()

	if needAutoCreate {
		a.createSession()
		return
	}
	if needFocus {
		a.focusSession(focusID)
	}
	a.window.Invalidate()
}

func (a *App) createSession() {
	if a.client == nil {
		return
	}
	cols, rows := a.currentPaneSize()
	info, err := a.client.Create("", "", cols, rows)
	if err != nil {
		a.setStatus("create failed: " + err.Error())
		return
	}
	a.mu.Lock()
	a.views[info.ID] = &sessionView{info: info}
	a.sessions = append(a.sessions, info)
	a.mu.Unlock()
	a.focusSession(info.ID)
	a.window.Invalidate()
}

func (a *App) killSession(id string) {
	if a.client == nil {
		return
	}
	a.mu.Lock()
	if v, ok := a.views[id]; ok {
		if v.stream != nil {
			_ = v.stream.Close()
		}
		delete(a.views, id)
	}
	if a.focusedID == id {
		a.focusedID = ""
	}
	a.mu.Unlock()
	_ = a.client.Kill(id)
	a.refreshSessions()
}

func (a *App) renameSession(id, name string) {
	if a.client == nil {
		return
	}
	if err := a.client.Rename(id, name); err != nil {
		a.setStatus("rename failed: " + err.Error())
		return
	}
	a.refreshSessions()
}

// --- context menu / rename UI state (event-loop goroutine only) ---

func (a *App) openMenu(sessionID string) {
	a.rename = nil
	a.menu = &menuState{sessionID: sessionID}
	a.window.Invalidate()
}

func (a *App) closeMenu() {
	a.menu = nil
	a.window.Invalidate()
}

func (a *App) startRename(sessionID, current string) {
	a.menu = nil
	ed := widget.Editor{SingleLine: true, Submit: true}
	ed.SetText(current)
	a.rename = &renameState{sessionID: sessionID, editor: ed}
	a.window.Invalidate()
}

func (a *App) commitRename() {
	if a.rename == nil {
		return
	}
	id, text := a.rename.sessionID, a.rename.editor.Text()
	a.rename = nil
	go a.renameSession(id, text)
}

func (a *App) cancelRename() {
	a.rename = nil
	a.window.Invalidate()
}

func (a *App) currentPaneSize() (cols, rows int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastCols > 0 && a.lastRows > 0 {
		return a.lastCols, a.lastRows
	}
	return 80, 24
}

func (a *App) focusSession(id string) {
	if a.client == nil {
		return
	}
	a.mu.Lock()
	v, ok := a.views[id]
	a.mu.Unlock()
	if !ok {
		return
	}

	cols, rows := a.currentPaneSize()

	a.mu.Lock()
	alreadyAttached := v.stream != nil
	a.focusedID = id
	a.mu.Unlock()

	if alreadyAttached {
		_ = a.client.Resize(id, cols, rows)
		if v.term != nil {
			v.term.Resize(cols, rows)
		}
		a.window.Invalidate()
		return
	}

	stream, ack, err := a.client.AttachStream(a.ctx, id, cols, rows)
	if err != nil {
		a.setStatus("attach failed: " + err.Error())
		return
	}
	term := vt10x.New(vt10x.WithSize(cols, rows))

	a.mu.Lock()
	v.stream = stream
	v.term = term
	a.mu.Unlock()

	a.setStatus(fmt.Sprintf("attached · replayed %d bytes", ack.ScrollbackLen))
	go a.readPump(id, stream, term)
	a.window.Invalidate()
}

func (a *App) readPump(id string, stream *quic.Stream, term vt10x.Terminal) {
	buf := make([]byte, 8*1024)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			_, _ = term.Write(buf[:n])
			a.window.Invalidate()
		}
		if err != nil {
			return
		}
	}
}

func (a *App) setStatus(s string) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
	a.window.Invalidate()
}

// focusedView returns the currently attached session's view, if any.
func (a *App) focusedView() (*sessionView, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.views[a.focusedID]
	return v, ok && v.term != nil
}

func (a *App) setPaneSize(cols, rows int) {
	a.mu.Lock()
	changed := cols != a.lastCols || rows != a.lastRows
	a.lastCols, a.lastRows = cols, rows
	id := a.focusedID
	v := a.views[id]
	a.mu.Unlock()

	if !changed || v == nil || v.term == nil {
		return
	}
	v.term.Resize(cols, rows)
	if a.client != nil {
		_ = a.client.Resize(id, cols, rows)
	}
}
