package main

import (
	"image/color"

	"github.com/hinshun/vt10x"
)

// App chrome palette — matches the Brilliant "multipx" design tokens so the
// GUI, TUI, and native-webview clients all read as the same product.
var (
	colSurface         = color.NRGBA{R: 0x24, G: 0x27, B: 0x2a, A: 0xff}
	colSurfaceContainer = color.NRGBA{R: 0x30, G: 0x33, B: 0x36, A: 0xff}
	colSurfaceHigh      = color.NRGBA{R: 0x39, G: 0x3e, B: 0x42, A: 0xff}
	colSurfaceSelected  = color.NRGBA{R: 0x00, G: 0x3a, B: 0x3a, A: 0xff}
	colOutlineVariant   = color.NRGBA{R: 0x39, G: 0x3e, B: 0x42, A: 0xff}
	colTextPrimary      = color.NRGBA{R: 0xdd, G: 0xde, B: 0xdf, A: 0xff}
	colTextSecondary    = color.NRGBA{R: 0xbd, G: 0xc0, B: 0xc3, A: 0xff}
	colTextDisabled     = color.NRGBA{R: 0x74, G: 0x7a, B: 0x7e, A: 0xff}
	colPrimary          = color.NRGBA{R: 0x00, G: 0xd4, B: 0xd5, A: 0xff}
	colOnPrimary        = color.NRGBA{R: 0x0a, G: 0x0a, B: 0x0a, A: 0xff}
	colWarning          = color.NRGBA{R: 0xff, G: 0x69, B: 0x00, A: 0xff}
	colError            = color.NRGBA{R: 0xfb, G: 0x2c, B: 0x36, A: 0xff}
	colSuccess          = color.NRGBA{R: 0x00, G: 0xc9, B: 0x50, A: 0xff}
)

// ansi16 is the classic xterm 16-color palette: indices 0-7 normal, 8-15
// bright. vt10x's bold-bright promotion (FG+8 for bold text under 8) means
// most "bold" color already arrives pre-resolved to the bright half.
var ansi16 = [16]color.NRGBA{
	{R: 0x1c, G: 0x1c, B: 0x1c, A: 0xff}, // black
	{R: 0xe0, G: 0x6c, B: 0x75, A: 0xff}, // red
	{R: 0x98, G: 0xc3, B: 0x79, A: 0xff}, // green
	{R: 0xe5, G: 0xc0, B: 0x7b, A: 0xff}, // yellow
	{R: 0x61, G: 0xaf, B: 0xef, A: 0xff}, // blue
	{R: 0xc6, G: 0x78, B: 0xdd, A: 0xff}, // magenta
	{R: 0x56, G: 0xb6, B: 0xc2, A: 0xff}, // cyan
	{R: 0xab, G: 0xb2, B: 0xbf, A: 0xff}, // white
	{R: 0x5c, G: 0x63, B: 0x70, A: 0xff}, // bright black
	{R: 0xe0, G: 0x6c, B: 0x75, A: 0xff}, // bright red
	{R: 0x98, G: 0xc3, B: 0x79, A: 0xff}, // bright green
	{R: 0xe5, G: 0xc0, B: 0x7b, A: 0xff}, // bright yellow
	{R: 0x61, G: 0xaf, B: 0xef, A: 0xff}, // bright blue
	{R: 0xc6, G: 0x78, B: 0xdd, A: 0xff}, // bright magenta
	{R: 0x56, G: 0xb6, B: 0xc2, A: 0xff}, // bright cyan
	{R: 0xff, G: 0xff, B: 0xff, A: 0xff}, // bright white
}

var cube6 = [6]byte{0, 95, 135, 175, 215, 255}

// termColor converts a vt10x color into an actual RGB color for GPU
// drawing. There is no library doing this for us here — unlike the TUI,
// which could hand ANSI indices to the host terminal and let it resolve
// them, we ARE the terminal now.
//
// vt10x.Color overloads one uint32 for two unrelated encodings (see its
// setAttr, case 38/48): a palette index (0-255, classic ANSI/xterm-256)
// for "ESC[38;5;Nm", or a packed 24-bit RGB value — Color(r<<16|g<<8|b)
// — for truecolor "ESC[38;2;r;g;bm", which is what most modern shells and
// prompts (PowerShell 7, starship, oh-my-posh, bat, delta, ...) actually
// emit today. The two encodings overlap in [0,255] (nothing distinguishes
// palette index 5 from truecolor rgb(0,0,5)); vt10x itself can't tell
// them apart either, so this has the same blind spot every caller of this
// library does. Above 255 there's no ambiguity, and that's the range that
// was silently swallowing every truecolor value into the default color.
func termColor(c vt10x.Color) color.NRGBA {
	switch c {
	case vt10x.DefaultFG:
		return colTextPrimary
	case vt10x.DefaultBG:
		return colSurface
	case vt10x.DefaultCursor:
		return colPrimary
	}
	n := uint32(c)
	switch {
	case n < 16:
		return ansi16[n]
	case n < 232:
		m := n - 16
		r := cube6[(m/36)%6]
		g := cube6[(m/6)%6]
		b := cube6[m%6]
		return color.NRGBA{R: r, G: g, B: b, A: 0xff}
	case n < 256:
		v := byte(8 + (n-232)*10)
		return color.NRGBA{R: v, G: v, B: v, A: 0xff}
	default:
		return color.NRGBA{R: byte(n >> 16), G: byte(n >> 8), B: byte(n), A: 0xff}
	}
}
