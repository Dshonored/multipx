package main

import (
	"fmt"

	"gioui.org/io/key"
	"gioui.org/io/pointer"

	"github.com/hinshun/vt10x"
)

// keyFilters is the fixed set of named/special keys the terminal pane wants
// delivered as discrete key.Event values. Plain character input (including
// shifted symbols and IME composition) arrives separately as key.EditEvent
// and needs no filter beyond being focused.
func keyFilters(tag any) []key.Filter {
	names := []key.Name{
		key.NameReturn, key.NameEnter, key.NameDeleteBackward, key.NameDeleteForward,
		key.NameTab, key.NameEscape, key.NameSpace,
		key.NameUpArrow, key.NameDownArrow, key.NameLeftArrow, key.NameRightArrow,
		key.NameHome, key.NameEnd, key.NamePageUp, key.NamePageDown,
	}
	filters := make([]key.Filter, 0, len(names)+26)
	for _, n := range names {
		filters = append(filters, key.Filter{Focus: tag, Name: n, Optional: key.ModShift | key.ModCtrl | key.ModAlt})
	}
	// Ctrl+<letter> shortcuts (Ctrl-C to interrupt, Ctrl-D to EOF, Ctrl-L to
	// clear, etc.) — every letter, since a terminal can't predict which
	// ones a given program will want.
	for c := 'A'; c <= 'Z'; c++ {
		filters = append(filters, key.Filter{Focus: tag, Name: key.Name(c), Required: key.ModCtrl, Optional: key.ModShift})
	}
	return filters
}

// keyEventToBytes translates a special-key event into the raw bytes a real
// terminal would send. Returns nil for keys that carry no byte sequence
// (e.g. a bare modifier) or aren't handled.
func keyEventToBytes(e key.Event) []byte {
	if e.State != key.Press {
		return nil
	}
	if e.Modifiers.Contain(key.ModCtrl) && len(e.Name) == 1 {
		c := e.Name[0]
		if c >= 'A' && c <= 'Z' {
			return []byte{c - 'A' + 1}
		}
	}
	switch e.Name {
	case key.NameReturn, key.NameEnter:
		return []byte("\r")
	case key.NameDeleteBackward:
		return []byte{0x7f}
	case key.NameDeleteForward:
		return []byte("\x1b[3~")
	case key.NameTab:
		if e.Modifiers.Contain(key.ModShift) {
			return []byte("\x1b[Z")
		}
		return []byte("\t")
	case key.NameEscape:
		return []byte{0x1b}
	case key.NameSpace:
		return []byte(" ")
	case key.NameUpArrow:
		return []byte("\x1b[A")
	case key.NameDownArrow:
		return []byte("\x1b[B")
	case key.NameRightArrow:
		return []byte("\x1b[C")
	case key.NameLeftArrow:
		return []byte("\x1b[D")
	case key.NameHome:
		return []byte("\x1b[H")
	case key.NameEnd:
		return []byte("\x1b[F")
	case key.NamePageUp:
		return []byte("\x1b[5~")
	case key.NamePageDown:
		return []byte("\x1b[6~")
	}
	return nil
}

// sgrMouseSeq encodes a click as an SGR (1006) mouse report.
func sgrMouseSeq(buttons pointer.Buttons, kind pointer.Kind, x, y int) string {
	var cb int
	switch {
	case buttons.Contain(pointer.ButtonPrimary):
		cb = 0
	case buttons.Contain(pointer.ButtonTertiary):
		cb = 1
	case buttons.Contain(pointer.ButtonSecondary):
		cb = 2
	default:
		if kind != pointer.Release {
			return ""
		}
		cb = 0
	}
	final := byte('M')
	if kind == pointer.Release {
		final = 'm'
	}
	return fmt.Sprintf("\x1b[<%d;%d;%d%c", cb, x+1, y+1, final)
}

func mouseModeActive(term vt10x.Terminal) bool {
	if term == nil {
		return false
	}
	term.Lock()
	mode := term.Mode()
	term.Unlock()
	return mode&vt10x.ModeMouseMask != 0
}
