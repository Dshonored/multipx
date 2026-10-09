package tui

import tea "github.com/charmbracelet/bubbletea"

// keyToBytes translates a bubbletea key event into the raw bytes a real
// terminal would have sent, so the focused session's shell sees exactly
// what it would over a direct connection. bubbletea's control-key KeyTypes
// are deliberately numbered to match their ASCII control codes (KeyCtrlC ==
// 3, KeyTab == 9, KeyEnter == 13, KeyEsc == 27, KeyBackspace == 127), so
// that whole range is just a cast.
func keyToBytes(msg tea.KeyMsg) []byte {
	switch msg.Type {
	case tea.KeyRunes:
		return []byte(string(msg.Runes))
	case tea.KeySpace:
		return []byte(" ")
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyInsert:
		return []byte("\x1b[2~")
	case tea.KeyShiftTab:
		return []byte("\x1b[Z")
	}
	// Control codes: 0-31 plus DEL (127) map 1:1 onto their KeyType value.
	if msg.Type >= 0 && msg.Type <= 31 || msg.Type == 127 {
		return []byte{byte(msg.Type)}
	}
	return nil
}
