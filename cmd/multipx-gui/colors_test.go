package main

import (
	"image/color"
	"testing"

	"github.com/hinshun/vt10x"
)

// TestTermColorPipeline isolates the color path from the shell/PTY: feed
// vt10x raw SGR-colored bytes directly and check that termColor resolves
// the expected RGB, so "no color" can be diagnosed as a shell/environment
// issue vs. an actual bug in our color handling.
func TestTermColorPipeline(t *testing.T) {
	term := vt10x.New(vt10x.WithSize(40, 5))
	_, _ = term.Write([]byte("\x1b[31mRED\x1b[0m \x1b[1;32mBOLDGREEN\x1b[0m plain"))

	term.Lock()
	defer term.Unlock()

	cases := []struct {
		x    int
		char rune
		want vt10x.Color
	}{
		{0, 'R', vt10x.Red},
		{1, 'E', vt10x.Red},
		{2, 'D', vt10x.Red},
		{4, 'B', vt10x.LightGreen}, // bold green gets promoted to bright per vt10x
	}
	for _, c := range cases {
		g := term.Cell(c.x, 0)
		if g.Char != c.char {
			t.Fatalf("cell %d: char = %q, want %q", c.x, g.Char, c.char)
		}
		if g.FG != c.want {
			t.Errorf("cell %d (%q): FG = %d, want %d", c.x, g.Char, g.FG, c.want)
		}
		got := termColor(g.FG)
		want := termColor(c.want)
		if got != want {
			t.Errorf("cell %d (%q): termColor(FG) = %+v, want %+v", c.x, g.Char, got, want)
		}
	}

	// Plain trailing text should resolve to the default foreground, not an
	// arbitrary color — and crucially must NOT equal the red/green used
	// above (that would indicate run-splitting isn't tracking color).
	plain := term.Cell(len("RED BOLDGREEN "), 0)
	if plain.FG != vt10x.DefaultFG {
		t.Errorf("plain text FG = %d, want DefaultFG (%d)", plain.FG, vt10x.DefaultFG)
	}
}

// TestTermColorTruecolor covers the bug that TestTermColorPipeline missed
// the first time: ESC[38;2;r;g;bm (truecolor), which is what PowerShell 7,
// starship, and most modern colorized tools actually emit — distinct from
// the indexed ESC[38;5;Nm form, and packed into the same vt10x.Color field
// in a completely different way (see termColor's doc comment).
func TestTermColorTruecolor(t *testing.T) {
	term := vt10x.New(vt10x.WithSize(20, 2))
	// Orange-ish truecolor foreground: r=255,g=136,b=0.
	_, _ = term.Write([]byte("\x1b[38;2;255;136;0mHI\x1b[0m"))

	term.Lock()
	defer term.Unlock()

	g := term.Cell(0, 0)
	if g.Char != 'H' {
		t.Fatalf("char = %q, want 'H'", g.Char)
	}
	want := color.NRGBA{R: 255, G: 136, B: 0, A: 0xff}
	got := termColor(g.FG)
	if got != want {
		t.Errorf("termColor(truecolor FG) = %+v, want %+v (raw vt10x.Color = %d)", got, want, g.FG)
	}
}
