// Package tui is the multipx terminal UI: a sidebar of sessions on one
// machine plus a single focused terminal pane, modeled after the
// Multipx/TUI design — all monospace, a tmux-style Ctrl-B prefix for
// multiplexer commands, everything else forwarded straight to the shell.
package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/hinshun/vt10x"
	"github.com/quic-go/quic-go"

	"multipx/internal/client"
	"multipx/internal/proto"
)

const (
	sidebarWidth = 28
	tickRate     = time.Second / 30
	refreshRate  = 2 * time.Second
)

type Model struct {
	client *client.Client
	ctx    context.Context

	sessions  []proto.SessionInfo
	focusedID string
	stream    *quic.Stream
	term      vt10x.Terminal

	width, height   int
	prefixArmed     bool
	autoCreateTried bool
	status          string
}

func New(ctx context.Context, c *client.Client) Model {
	return Model{client: c, ctx: ctx}
}

// Close releases the focused stream and the daemon connection. Call it
// after the bubbletea program exits; sessions keep running on the daemon
// regardless.
func (m Model) Close() {
	if m.stream != nil {
		_ = m.stream.Close()
	}
	_ = m.client.Close()
}

// --- messages ---

type sessionListMsg struct {
	sessions []proto.SessionInfo
	err      error
}
type refreshNowMsg struct{}
type tickMsg time.Time
type newSessionMsg struct {
	session proto.SessionInfo
	err     error
}
type killedMsg struct{ id string }
type attachedMsg struct {
	sessionID     string
	stream        *quic.Stream
	term          vt10x.Terminal
	scrollbackLen int
	err           error
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetchSessionsCmd(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickRate, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) fetchSessionsCmd() tea.Cmd {
	return func() tea.Msg {
		sessions, err := m.client.List()
		return sessionListMsg{sessions: sessions, err: err}
	}
}

func (m Model) attachCmd(sessionID string, cols, rows int) tea.Cmd {
	return func() tea.Msg {
		stream, ok, err := m.client.AttachStream(m.ctx, sessionID, cols, rows)
		if err != nil {
			return attachedMsg{sessionID: sessionID, err: err}
		}
		term := vt10x.New(vt10x.WithSize(cols, rows))
		return attachedMsg{
			sessionID:     sessionID,
			stream:        stream,
			term:          term,
			scrollbackLen: ok.ScrollbackLen,
		}
	}
}

func (m Model) newSessionCmd() tea.Cmd {
	cols, rows := m.mainPaneSize()
	return func() tea.Msg {
		s, err := m.client.Create("", "", cols, rows)
		return newSessionMsg{session: s, err: err}
	}
}

func (m Model) killFocusedCmd() tea.Cmd {
	id := m.focusedID
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		_ = m.client.Kill(id)
		return killedMsg{id: id}
	}
}

// readPump feeds live output into term until the stream errors (detach or
// session exit). gen guards against a stale pump from a prior attach ever
// mattering: by the time it exits, nobody renders its term anymore.
func (m Model) readPump(stream *quic.Stream, term vt10x.Terminal) {
	buf := make([]byte, 8*1024)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			_, _ = term.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// --- update ---

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		firstSize := m.width == 0
		m.width, m.height = msg.Width, msg.Height
		cols, rows := m.mainPaneSize()
		if m.term != nil {
			m.term.Resize(cols, rows)
			if m.focusedID != "" {
				_ = m.client.Resize(m.focusedID, cols, rows)
			}
		}
		if firstSize && m.focusedID == "" && len(m.sessions) > 0 {
			return m, m.attachCmd(m.sessions[0].ID, cols, rows)
		}
		return m, nil

	case sessionListMsg:
		if msg.err == nil {
			m.sessions = msg.sessions
			if len(m.sessions) == 0 && !m.autoCreateTried && m.width > 0 {
				// Nothing to attach to yet: open a first session by default
				// instead of greeting the user with an empty sidebar.
				m.autoCreateTried = true
				return m, tea.Batch(m.newSessionCmd(), scheduleRefresh())
			}
			if m.focusedID == "" && len(m.sessions) > 0 && m.width > 0 {
				cols, rows := m.mainPaneSize()
				return m, tea.Batch(m.attachCmd(m.sessions[0].ID, cols, rows), scheduleRefresh())
			}
		}
		return m, scheduleRefresh()

	case refreshNowMsg:
		return m, m.fetchSessionsCmd()

	case tickMsg:
		return m, tickCmd()

	case newSessionMsg:
		if msg.err != nil {
			m.status = "create failed: " + msg.err.Error()
			return m, nil
		}
		m.sessions = append(m.sessions, msg.session)
		cols, rows := m.mainPaneSize()
		return m, m.attachCmd(msg.session.ID, cols, rows)

	case killedMsg:
		if msg.id == m.focusedID {
			if m.stream != nil {
				_ = m.stream.Close()
			}
			m.stream, m.term, m.focusedID = nil, nil, ""
		}
		return m, m.fetchSessionsCmd()

	case attachedMsg:
		if msg.err != nil {
			m.status = "attach failed: " + msg.err.Error()
			return m, nil
		}
		if m.stream != nil {
			_ = m.stream.Close()
		}
		m.stream, m.term, m.focusedID = msg.stream, msg.term, msg.sessionID
		m.status = fmt.Sprintf("attached · replayed %d bytes", msg.scrollbackLen)
		go m.readPump(msg.stream, msg.term)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(tea.MouseEvent(msg))
	}
	return m, nil
}

func scheduleRefresh() tea.Cmd {
	return tea.Tick(refreshRate, func(time.Time) tea.Msg { return refreshNowMsg{} })
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.prefixArmed {
		m.prefixArmed = false
		switch {
		case msg.Type == tea.KeyCtrlB:
			if m.stream != nil {
				_, _ = m.stream.Write([]byte{0x02})
			}
		case msg.String() == "d":
			return m, tea.Quit
		case msg.String() == "n":
			return m, m.newSessionCmd()
		case msg.String() == "x":
			return m, m.killFocusedCmd()
		case msg.Type == tea.KeyUp || msg.String() == "k":
			if id, ok := m.neighborID(-1); ok {
				cols, rows := m.mainPaneSize()
				return m, m.attachCmd(id, cols, rows)
			}
		case msg.Type == tea.KeyDown || msg.String() == "j":
			if id, ok := m.neighborID(1); ok {
				cols, rows := m.mainPaneSize()
				return m, m.attachCmd(id, cols, rows)
			}
		}
		return m, nil
	}

	if msg.Type == tea.KeyCtrlB {
		m.prefixArmed = true
		return m, nil
	}

	if m.stream != nil {
		if b := keyToBytes(msg); b != nil {
			_, _ = m.stream.Write(b)
		}
	}
	return m, nil
}

// sidebarHeaderRows is how many lines renderSidebar draws before the first
// session row, so mouse Y can be mapped back to a session index.
const sidebarHeaderRows = 1

// handleMouse implements the two things a real terminal multiplexer needs
// from the mouse: clicking a sidebar row to switch to it (left) or kill it
// (right), and — the part that actually makes the main pane "feel like a
// real terminal" — forwarding clicks through to whatever is running in the
// focused session, exactly like tmux does, but only once that program has
// asked for mouse reports (so a plain shell prompt doesn't see garbage
// escape sequences from an idle mouse move).
func (m Model) handleMouse(ev tea.MouseEvent) (tea.Model, tea.Cmd) {
	if ev.X < sidebarWidth {
		return m.handleSidebarClick(ev)
	}
	return m.forwardMouseToPane(ev)
}

func (m Model) handleSidebarClick(ev tea.MouseEvent) (tea.Model, tea.Cmd) {
	if ev.Action != tea.MouseActionPress {
		return m, nil
	}
	row := ev.Y - sidebarHeaderRows
	if row < 0 || row >= len(m.sessions) {
		return m, nil
	}
	s := m.sessions[row]
	switch ev.Button {
	case tea.MouseButtonLeft:
		cols, rows := m.mainPaneSize()
		return m, m.attachCmd(s.ID, cols, rows)
	case tea.MouseButtonRight:
		m.status = fmt.Sprintf("killed %s (right-click)", s.Name)
		return m, func() tea.Msg {
			_ = m.client.Kill(s.ID)
			return killedMsg{id: s.ID}
		}
	}
	return m, nil
}

func (m Model) forwardMouseToPane(ev tea.MouseEvent) (tea.Model, tea.Cmd) {
	if m.term == nil || m.stream == nil {
		return m, nil
	}
	m.term.Lock()
	mode := m.term.Mode()
	m.term.Unlock()
	if mode&vt10x.ModeMouseMask == 0 {
		return m, nil // the focused program never asked for mouse reports
	}
	paneX := ev.X - sidebarWidth - 1
	paneY := ev.Y - 1 // header line
	if paneX < 0 || paneY < 0 {
		return m, nil
	}
	if seq := sgrMouseSeq(ev, paneX, paneY); seq != "" {
		_, _ = m.stream.Write([]byte(seq))
	}
	return m, nil
}

// sgrMouseSeq encodes a click as an SGR (1006) mouse report, the modern
// extended mouse protocol every mouse-aware terminal program understands.
func sgrMouseSeq(ev tea.MouseEvent, x, y int) string {
	var cb int
	switch ev.Button {
	case tea.MouseButtonLeft:
		cb = 0
	case tea.MouseButtonMiddle:
		cb = 1
	case tea.MouseButtonRight:
		cb = 2
	case tea.MouseButtonWheelUp:
		cb = 64
	case tea.MouseButtonWheelDown:
		cb = 65
	default:
		return ""
	}
	if ev.Action == tea.MouseActionMotion {
		cb += 32
	}
	final := byte('M')
	if ev.Action == tea.MouseActionRelease {
		final = 'm'
	}
	return fmt.Sprintf("\x1b[<%d;%d;%d%c", cb, x+1, y+1, final)
}

func (m Model) currentIndex() int {
	for i, s := range m.sessions {
		if s.ID == m.focusedID {
			return i
		}
	}
	return -1
}

func (m Model) neighborID(delta int) (string, bool) {
	if len(m.sessions) == 0 {
		return "", false
	}
	i := m.currentIndex()
	if i < 0 {
		return m.sessions[0].ID, true
	}
	i = ((i+delta)%len(m.sessions) + len(m.sessions)) % len(m.sessions)
	return m.sessions[i].ID, true
}

func (m Model) findSession(id string) (proto.SessionInfo, bool) {
	for _, s := range m.sessions {
		if s.ID == id {
			return s, true
		}
	}
	return proto.SessionInfo{}, false
}

func (m Model) mainPaneSize() (cols, rows int) {
	cols = m.width - sidebarWidth - 1
	rows = m.height - 2 // header line + status bar
	if cols < 10 {
		cols = 10
	}
	if rows < 3 {
		rows = 3
	}
	return
}

// --- view ---

func (m Model) View() string {
	if m.width == 0 {
		return "starting…"
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(), m.renderMain())
	return lipgloss.JoinVertical(lipgloss.Left, body, m.renderStatusBar())
}

func (m Model) renderSidebar() string {
	height := m.height - 1
	var b strings.Builder

	headerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true)
	b.WriteString(headerStyle.Render(" SESSIONS"))
	b.WriteString("\n")

	if len(m.sessions) == 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(" ^B n to create one"))
	}

	for _, s := range m.sessions {
		dotColor := "2" // running: green
		if s.Status != proto.StatusRunning {
			dotColor = "8" // exited: dim grey
		}
		dot := lipgloss.NewStyle().Foreground(lipgloss.Color(dotColor)).Render("●")
		row := fmt.Sprintf(" %s %s", dot, s.Name)
		style := lipgloss.NewStyle().Width(sidebarWidth)
		if s.ID == m.focusedID {
			style = style.Background(lipgloss.Color("23")).Foreground(lipgloss.Color("15")).Bold(true)
		}
		b.WriteString(style.Render(row))
		b.WriteString("\n")
	}

	return lipgloss.NewStyle().
		Width(sidebarWidth).
		Height(height).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("8")).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(true).
		Render(b.String())
}

func (m Model) renderMain() string {
	cols, rows := m.mainPaneSize()
	var b strings.Builder

	header := "no session attached"
	if s, ok := m.findSession(m.focusedID); ok {
		header = fmt.Sprintf("%s  ·  %s", s.Name, s.Shell)
	}
	b.WriteString(lipgloss.NewStyle().Bold(true).Width(cols).Render(header))
	b.WriteString("\n")

	if m.term != nil {
		m.term.Lock()
		for y := range rows {
			b.WriteString(renderTermRow(m.term, y, cols))
			b.WriteString("\n")
		}
		m.term.Unlock()
	} else {
		b.WriteString(strings.Repeat("\n", rows))
	}

	return lipgloss.NewStyle().Width(cols).Height(m.height - 1).Render(b.String())
}

// cellStyle is the comparable subset of a cell's rendering attributes, used
// to batch consecutive same-styled cells into one styled span instead of
// emitting a fresh SGR set/reset per character. A real terminal's own
// renderer does this kind of run coalescing; without it a frame is
// hundreds of tiny escape sequences and tends to render as flicker/garbage
// on consoles with weaker VT processing (notably older Windows hosts).
type cellStyle struct {
	fg, bg     lipgloss.Color
	fgOK, bgOK bool
}

func renderTermRow(term vt10x.Terminal, y, cols int) string {
	var out, run strings.Builder
	var cur cellStyle
	haveRun := false

	flush := func() {
		if !haveRun {
			return
		}
		style := lipgloss.NewStyle()
		if cur.fgOK {
			style = style.Foreground(cur.fg)
		}
		if cur.bgOK {
			style = style.Background(cur.bg)
		}
		out.WriteString(style.Render(run.String()))
		run.Reset()
	}

	for x := range cols {
		g := term.Cell(x, y)
		fg, fgOK := convColor(g.FG)
		bg, bgOK := convColor(g.BG)
		cs := cellStyle{fg: fg, fgOK: fgOK, bg: bg, bgOK: bgOK}
		if haveRun && cs != cur {
			flush()
		}
		cur = cs
		haveRun = true
		ch := g.Char
		if ch == 0 {
			ch = ' '
		}
		run.WriteRune(ch)
	}
	flush()
	return out.String()
}

func convColor(c vt10x.Color) (lipgloss.Color, bool) {
	switch c {
	case vt10x.DefaultFG, vt10x.DefaultBG, vt10x.DefaultCursor:
		return "", false
	}
	return lipgloss.Color(strconv.Itoa(int(c))), true
}

func (m Model) renderStatusBar() string {
	left := " ^B n new   ^B x kill   ^B ↑↓ switch   ^B d quit"
	right := m.status
	if right != "" {
		right += " "
	}
	style := lipgloss.NewStyle().Background(lipgloss.Color("6")).Foreground(lipgloss.Color("0"))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return style.Width(m.width).Render(left + strings.Repeat(" ", gap) + right)
}
