package main

import (
	"fmt"
	"image"
	"image/color"
	"strings"

	"gioui.org/app"
	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/hinshun/vt10x"

	"multipx/internal/proto"
)

const (
	sidebarWidthDp = unit.Dp(240)
	topbarHeightDp = unit.Dp(44)
	statusHeightDp = unit.Dp(26)
	rowHeightDp    = unit.Dp(28)
	fontSizeSp     = unit.Sp(13)
)

// loose returns gtx with Min reset to zero and Max capped to (maxW, maxH).
// Every Label.Layout call in this file goes through this: Gio's text
// shaper takes gtx.Constraints.Min.X as a hard MinWidth for the shaped
// line (see widget/label.go), and app.NewContext hands the FULL WINDOW
// size as both Min and Max. Without this, every label we draw — topbar
// text, sidebar names, every colored run in the terminal grid — was being
// shaped against a multi-hundred-pixel minimum width it was never meant
// to have.
func loose(gtx layout.Context, maxW, maxH int) layout.Context {
	gtx.Constraints = layout.Constraints{Max: image.Pt(maxW, maxH)}
	return gtx
}

func (a *App) EventLoop() error {
	var ops op.Ops
	for {
		switch e := a.window.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			a.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func (a *App) layout(gtx layout.Context) {
	paint.Fill(gtx.Ops, colSurface)

	statusH := gtx.Dp(statusHeightDp)
	topH := gtx.Dp(topbarHeightDp)

	// Topbar
	func() {
		defer clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, topH)}.Push(gtx.Ops).Pop()
		a.layoutTopbar(gtx, topH)
	}()

	// Status bar (bottom)
	statusOff := op.Offset(image.Pt(0, gtx.Constraints.Max.Y-statusH)).Push(gtx.Ops)
	a.layoutStatusBar(gtx, statusH)
	statusOff.Pop()

	// Body: sidebar + main, between topbar and status bar
	bodyOff := op.Offset(image.Pt(0, topH)).Push(gtx.Ops)
	bodyH := gtx.Constraints.Max.Y - topH - statusH
	sideW := gtx.Dp(sidebarWidthDp)

	func() {
		defer clip.Rect{Max: image.Pt(sideW, bodyH)}.Push(gtx.Ops).Pop()
		a.layoutSidebar(gtx, sideW, bodyH)
	}()

	mainOff := op.Offset(image.Pt(sideW, 0)).Push(gtx.Ops)
	a.layoutMain(gtx, gtx.Constraints.Max.X-sideW, bodyH)
	mainOff.Pop()
	bodyOff.Pop()

	if a.menu != nil {
		a.layoutContextMenu(gtx, sideW, topH)
	}
}

var (
	menuRenameTag = new(int)
	menuCloseTag  = new(int)
)

// layoutContextMenu draws the right-click menu for whichever session
// a.menu names. It's anchored under the sidebar rather than at the exact
// click point — simpler to get right, and the menu always names the
// session it targets so there's no ambiguity about what it acts on.
func (a *App) layoutContextMenu(gtx layout.Context, sideW, topH int) {
	a.mu.Lock()
	v, ok := a.views[a.menu.sessionID]
	var sessionID, name string
	if ok {
		sessionID, name = v.info.ID, v.info.Name
	}
	a.mu.Unlock()
	if !ok {
		a.menu = nil
		return
	}

	const w, h, rowH = 160, 76, 32
	x, y := 8, topH+8

	for {
		ev, ok := gtx.Event(key.Filter{Focus: menuRenameTag, Name: key.NameEscape})
		if !ok {
			break
		}
		if _, ok := ev.(key.Event); ok {
			a.closeMenu()
			return
		}
	}
	gtx.Execute(key.FocusCmd{Tag: menuRenameTag})

	off := op.Offset(image.Pt(x, y)).Push(gtx.Ops)
	defer off.Pop()

	area := clip.Rect{Max: image.Pt(w, h)}
	paint.FillShape(gtx.Ops, colSurfaceHigh, area.Op())
	paint.FillShape(gtx.Ops, colOutlineVariant, clip.Rect{Max: image.Pt(w, 1)}.Op())

	titleOff := op.Offset(image.Pt(10, 6)).Push(gtx.Ops)
	title := material.Label(a.th, unit.Sp(11), truncate(name, 18))
	title.Color = colTextDisabled
	title.Layout(loose(gtx, w-20, 16))
	titleOff.Pop()

	a.layoutMenuItem(gtx, menuRenameTag, "Rename", 0, rowH, w, func() {
		a.startRename(sessionID, name)
	})
	a.layoutMenuItem(gtx, menuCloseTag, "Close session", rowH, rowH, w, func() {
		a.closeMenu()
		go a.killSession(sessionID)
	})
}

func (a *App) layoutMenuItem(gtx layout.Context, tag event.Tag, text string, yOff, h, w int, onClick func()) {
	rOff := op.Offset(image.Pt(0, 24+yOff)).Push(gtx.Ops)
	area := clip.Rect{Max: image.Pt(w, h)}

	for {
		ev, ok := gtx.Event(pointer.Filter{Target: tag, Kinds: pointer.Press})
		if !ok {
			break
		}
		if _, ok := ev.(pointer.Event); ok {
			onClick()
		}
	}

	stack := area.Push(gtx.Ops)
	event.Op(gtx.Ops, tag)
	stack.Pop()

	lblOff := op.Offset(image.Pt(10, h/2-9)).Push(gtx.Ops)
	lbl := material.Label(a.th, unit.Sp(13), text)
	lbl.Color = colTextPrimary
	lbl.Layout(loose(gtx, w-20, 18))
	lblOff.Pop()

	rOff.Pop()
}

func (a *App) layoutTopbar(gtx layout.Context, h int) {
	paint.FillShape(gtx.Ops, colSurfaceContainer, clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, h)}.Op())
	paint.FillShape(gtx.Ops, colOutlineVariant, clip.Rect{Min: image.Pt(0, h-1), Max: image.Pt(gtx.Constraints.Max.X, h)}.Op())

	off := op.Offset(image.Pt(16, (h-18)/2)).Push(gtx.Ops)
	lbl := material.Label(a.th, unit.Sp(15), "multipx")
	lbl.Font = font.Font{Typeface: "monospace", Weight: font.Bold}
	lbl.Color = colTextPrimary
	lbl.Layout(loose(gtx, gtx.Constraints.Max.X, h))
	off.Pop()

	a.mu.Lock()
	status := a.connStatusText()
	a.mu.Unlock()
	drawRightAligned(gtx, status, gtx.Constraints.Max.X-12, (h-16)/2, a.th, colTextDisabled)
}

func (a *App) connStatusText() string {
	if a.client == nil {
		if a.connErr != nil {
			return "daemon unreachable: " + a.connErr.Error()
		}
		return "connecting…"
	}
	if a.connErr != nil {
		return "daemon unreachable"
	}
	return "connected"
}

func (a *App) layoutStatusBar(gtx layout.Context, h int) {
	w := gtx.Constraints.Max.X
	paint.FillShape(gtx.Ops, colSurfaceContainer, clip.Rect{Max: image.Pt(w, h)}.Op())

	a.mu.Lock()
	status := a.status
	a.mu.Unlock()
	if status == "" {
		status = "ready"
	}
	off := op.Offset(image.Pt(12, (h-14)/2)).Push(gtx.Ops)
	lbl := material.Label(a.th, unit.Sp(11), status)
	lbl.Font = font.Font{Typeface: "monospace"}
	lbl.Color = colTextDisabled
	lbl.Layout(loose(gtx, w, h))
	off.Pop()
}

func (a *App) layoutSidebar(gtx layout.Context, w, h int) {
	paint.FillShape(gtx.Ops, colSurfaceContainer, clip.Rect{Max: image.Pt(w, h)}.Op())
	paint.FillShape(gtx.Ops, colOutlineVariant, clip.Rect{Min: image.Pt(w-1, 0), Max: image.Pt(w, h)}.Op())

	a.mu.Lock()
	sessions := append([]proto.SessionInfo(nil), a.sessions...)
	focusedID := a.focusedID
	a.mu.Unlock()

	off := op.Offset(image.Pt(0, 8)).Push(gtx.Ops)
	defer off.Pop()

	hdr := material.Label(a.th, unit.Sp(11), "SESSIONS")
	hdr.Font = font.Font{Typeface: "monospace", Weight: font.Bold}
	hdr.Color = colTextDisabled
	ho := op.Offset(image.Pt(10, 0)).Push(gtx.Ops)
	hdr.Layout(loose(gtx, w, 16))
	ho.Pop()

	rowH := gtx.Dp(rowHeightDp)
	y := gtx.Dp(unit.Dp(22))

	for _, info := range sessions {
		a.layoutSessionRow(gtx, info, info.ID == focusedID, w, y, rowH)
		y += rowH + gtx.Dp(unit.Dp(2))
	}

	a.layoutAddRow(gtx, w, y, rowH)
}

func (a *App) layoutSessionRow(gtx layout.Context, info proto.SessionInfo, focused bool, w, y, h int) {
	a.mu.Lock()
	v := a.views[info.ID]
	a.mu.Unlock()
	if v == nil {
		return
	}

	rowOff := op.Offset(image.Pt(4, y)).Push(gtx.Ops)
	rowW := w - 8
	area := clip.Rect{Max: image.Pt(rowW, h)}

	// Process events queued against this row's tag from the previous frame.
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: v, Kinds: pointer.Press})
		if !ok {
			break
		}
		if pe, ok := ev.(pointer.Event); ok {
			if pe.Buttons.Contain(pointer.ButtonSecondary) {
				a.openMenu(info.ID)
			} else {
				a.closeMenu()
				a.cancelRename()
				go a.focusSession(info.ID)
			}
		}
	}

	if focused {
		paint.FillShape(gtx.Ops, colSurfaceSelected, area.Op())
	}

	stack := area.Push(gtx.Ops)
	event.Op(gtx.Ops, v)
	stack.Pop()

	dotColor := colTextDisabled
	if info.Status == proto.StatusRunning {
		dotColor = colSuccess
	}
	dotOff := op.Offset(image.Pt(10, h/2-3)).Push(gtx.Ops)
	paint.FillShape(gtx.Ops, dotColor, clip.Ellipse{Max: image.Pt(7, 7)}.Op(gtx.Ops))
	dotOff.Pop()

	nameOff := op.Offset(image.Pt(26, h/2-9)).Push(gtx.Ops)
	if a.rename != nil && a.rename.sessionID == info.ID {
		a.layoutRenameEditor(gtx, rowW-26)
	} else {
		weight := font.Normal
		if focused {
			weight = font.Bold
		}
		lbl := material.Label(a.th, unit.Sp(13), truncate(info.Name, 24))
		lbl.Font = font.Font{Weight: weight}
		lbl.Color = colTextPrimary
		lbl.Layout(loose(gtx, rowW-26, 18))
	}
	nameOff.Pop()

	rowOff.Pop()
}

// layoutRenameEditor draws the inline rename field and drains its own
// events: Enter commits, Escape cancels. These aren't things widget.Editor
// watches for itself (Submit only fires on its own, Escape isn't in its
// filter set at all), so we register them against the same tag ourselves.
func (a *App) layoutRenameEditor(gtx layout.Context, w int) {
	r := a.rename
	if r == nil {
		return
	}
	gtx.Execute(key.FocusCmd{Tag: &r.editor})

	for {
		ev, ok := gtx.Event(key.Filter{Focus: &r.editor, Name: key.NameEscape})
		if !ok {
			break
		}
		if _, ok := ev.(key.Event); ok {
			a.cancelRename()
			return
		}
	}

	ed := material.Editor(a.th, &r.editor, "")
	ed.Color = colTextPrimary
	ed.SelectionColor = colSurfaceSelected
	ed.Layout(loose(gtx, w, 18))

	for {
		e, ok := r.editor.Update(gtx)
		if !ok {
			break
		}
		if _, ok := e.(widget.SubmitEvent); ok {
			a.commitRename()
			return
		}
	}
}

func (a *App) layoutAddRow(gtx layout.Context, w, y, h int) {
	rowOff := op.Offset(image.Pt(4, y)).Push(gtx.Ops)
	rowW := w - 8
	area := clip.Rect{Max: image.Pt(rowW, h)}

	for {
		ev, ok := gtx.Event(pointer.Filter{Target: addRowTag, Kinds: pointer.Press})
		if !ok {
			break
		}
		if _, ok := ev.(pointer.Event); ok {
			a.closeMenu()
			a.cancelRename()
			go a.createSession()
		}
	}

	stack := area.Push(gtx.Ops)
	event.Op(gtx.Ops, addRowTag)
	stack.Pop()

	lbl := material.Label(a.th, unit.Sp(12), "+ new session")
	lbl.Color = colTextSecondary
	lo := op.Offset(image.Pt(10, h/2-9)).Push(gtx.Ops)
	lbl.Layout(loose(gtx, rowW-10, 18))
	lo.Pop()

	rowOff.Pop()
}

func (a *App) layoutMain(gtx layout.Context, w, h int) {
	v, hasTerm := a.focusedView()

	headerH := gtx.Dp(unit.Dp(32))
	paint.FillShape(gtx.Ops, colOutlineVariant, clip.Rect{Min: image.Pt(0, headerH - 1), Max: image.Pt(w, headerH)}.Op())

	if v != nil {
		a.mu.Lock()
		info := v.info
		a.mu.Unlock()
		hOff := op.Offset(image.Pt(12, headerH/2-9)).Push(gtx.Ops)
		lbl := material.Label(a.th, unit.Sp(13), fmt.Sprintf("%s  ·  %s", info.Name, info.Shell))
		lbl.Font = font.Font{Weight: font.Bold}
		lbl.Color = colTextPrimary
		lbl.Layout(loose(gtx, w-12, 18))
		hOff.Pop()
	}

	paneOff := op.Offset(image.Pt(8, headerH+4)).Push(gtx.Ops)
	paneW, paneH := w-16, h-headerH-8
	a.layoutTermPane(gtx, paneW, paneH, v, hasTerm)
	paneOff.Pop()
}

func (a *App) layoutTermPane(gtx layout.Context, w, h int, v *sessionView, hasTerm bool) {
	if w <= 0 || h <= 0 {
		return
	}
	area := clip.Rect{Max: image.Pt(w, h)}

	cellW, cellH := a.measureCell(gtx)
	if cellW <= 0 {
		cellW = 8
	}
	if cellH <= 0 {
		cellH = 16
	}
	cols, rows := w/cellW, h/cellH
	if cols < 10 {
		cols = 10
	}
	if rows < 3 {
		rows = 3
	}
	a.setPaneSize(cols, rows)

	// Always keep focus on the pane (even with no session attached yet) so
	// typing works without an extra click, and so Ctrl+N works from the
	// empty state too.
	gtx.Execute(key.FocusCmd{Tag: paneTag})

	a.handlePaneEvents(gtx, v, hasTerm)

	stack := area.Push(gtx.Ops)
	event.Op(gtx.Ops, paneTag)
	stack.Pop()

	if !hasTerm {
		lbl := material.Label(a.th, unit.Sp(13), "no session attached")
		lbl.Color = colTextDisabled
		lbl.Layout(loose(gtx, w, h))
		return
	}

	v.term.Lock()
	termCols, termRows := v.term.Size()
	visRows := rows
	if termRows < visRows {
		visRows = termRows
	}
	visCols := cols
	if termCols < visCols {
		visCols = termCols
	}
	for y := 0; y < visRows; y++ {
		a.drawTermRow(gtx, v.term, y, visCols, cellW, cellH)
	}
	cur := v.term.Cursor()
	curVisible := v.term.CursorVisible()
	v.term.Unlock()

	if curVisible && cur.X < cols && cur.Y < rows {
		cursorCol := colPrimary
		cursorCol.A = 150
		co := op.Offset(image.Pt(cur.X*cellW, cur.Y*cellH)).Push(gtx.Ops)
		paint.FillShape(gtx.Ops, cursorCol, clip.Rect{Max: image.Pt(cellW, cellH)}.Op())
		co.Pop()
	}
}

func (a *App) drawTermRow(gtx layout.Context, term vt10x.Terminal, y, cols, cellW, cellH int) {
	type run struct {
		fg, bg vt10x.Color
		text   strings.Builder
		start  int
	}
	var runs []*run
	var cur *run

	for x := 0; x < cols; x++ {
		g := term.Cell(x, y)
		if cur == nil || cur.fg != g.FG || cur.bg != g.BG {
			cur = &run{fg: g.FG, bg: g.BG, start: x}
			runs = append(runs, cur)
		}
		ch := g.Char
		if ch == 0 {
			ch = ' '
		}
		cur.text.WriteRune(ch)
	}

	for _, r := range runs {
		txt := r.text.String()
		runW := len([]rune(txt)) * cellW
		bg := termColor(r.bg)
		if bg != colSurface {
			bgOff := op.Offset(image.Pt(r.start*cellW, y*cellH)).Push(gtx.Ops)
			paint.FillShape(gtx.Ops, bg, clip.Rect{Max: image.Pt(runW, cellH)}.Op())
			bgOff.Pop()
		}
		if strings.TrimSpace(txt) == "" {
			continue
		}
		fgOff := op.Offset(image.Pt(r.start*cellW, y*cellH)).Push(gtx.Ops)
		lbl := material.Label(a.th, fontSizeSp, txt)
		lbl.Font = font.Font{Typeface: "monospace"}
		lbl.Color = termColor(r.fg)
		lbl.Layout(loose(gtx, runW, cellH))
		fgOff.Pop()
	}
}

// handlePaneEvents drains key and pointer events queued against paneTag
// since the last frame and forwards them to the attached session's PTY.
func (a *App) handlePaneEvents(gtx layout.Context, v *sessionView, hasTerm bool) {
	filters := append([]event.Filter{
		key.FocusFilter{Target: paneTag},
		pointer.Filter{Target: paneTag, Kinds: pointer.Press | pointer.Release},
	}, filtersAsEvent(keyFilters(paneTag))...)

	for {
		ev, ok := gtx.Event(filters...)
		if !ok {
			break
		}
		if ke, ok := ev.(key.Event); ok && ke.State == key.Press &&
			ke.Name == "N" && ke.Modifiers.Contain(key.ModCtrl) {
			go a.createSession()
			continue
		}
		if !hasTerm || v == nil {
			continue
		}
		switch e := ev.(type) {
		case key.EditEvent:
			_, _ = v.stream.Write([]byte(e.Text))
		case key.Event:
			if b := keyEventToBytes(e); b != nil {
				_, _ = v.stream.Write(b)
			}
		case pointer.Event:
			if mouseModeActive(v.term) {
				cellW, cellH := a.measureCell(gtx)
				if cellW <= 0 {
					cellW = 8
				}
				if cellH <= 0 {
					cellH = 16
				}
				px, py := int(e.Position.X)/cellW, int(e.Position.Y)/cellH
				kind := pointer.Press
				if e.Kind == pointer.Release {
					kind = pointer.Release
				}
				if seq := sgrMouseSeq(e.Buttons, kind, px, py); seq != "" {
					_, _ = v.stream.Write([]byte(seq))
				}
			}
		}
	}
}

func filtersAsEvent(fs []key.Filter) []event.Filter {
	out := make([]event.Filter, len(fs))
	for i, f := range fs {
		out[i] = f
	}
	return out
}

// measureCell returns the pixel size of one monospace glyph cell by
// shaping a representative character into a scratch ops buffer (so the
// measurement never touches the real frame's visible output).
func (a *App) measureCell(gtx layout.Context) (w, h int) {
	scratch := new(op.Ops)
	mgtx := gtx
	mgtx.Ops = scratch
	mgtx.Constraints = layout.Constraints{Max: image.Pt(1<<20, 1<<20)}
	lbl := material.Label(a.th, fontSizeSp, "M")
	lbl.Font = font.Font{Typeface: "monospace"}
	dims := lbl.Layout(mgtx)
	return dims.Size.X, dims.Size.Y
}

func drawRightAligned(gtx layout.Context, text string, rightX, y int, th *material.Theme, col color.NRGBA) {
	scratch := new(op.Ops)
	mgtx := gtx
	mgtx.Ops = scratch
	mgtx.Constraints = layout.Constraints{Max: image.Pt(1<<20, 1<<20)}
	lbl := material.Label(th, unit.Sp(11), text)
	lbl.Font = font.Font{Typeface: "monospace"}
	dims := lbl.Layout(mgtx)

	off := op.Offset(image.Pt(rightX-dims.Size.X, y)).Push(gtx.Ops)
	real := material.Label(th, unit.Sp(11), text)
	real.Font = font.Font{Typeface: "monospace"}
	real.Color = col
	real.Layout(loose(gtx, dims.Size.X, dims.Size.Y))
	off.Pop()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
