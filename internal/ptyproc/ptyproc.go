// Package ptyproc spawns a shell under a real pseudo-terminal so interactive
// programs (shells, editors, REPLs, agent CLIs) behave exactly as they
// would in a native terminal: line editing, color, cursor control, resize.
//
// Proc is implemented per-OS (ptyproc_windows.go today; a creack/pty-backed
// Unix implementation is the natural next addition behind a build tag).
package ptyproc

import "io"

// Proc is a running shell attached to a pseudo-terminal.
type Proc interface {
	io.ReadWriter
	Resize(cols, rows int) error
	Close() error
	// Wait blocks until the process exits and returns its exit code.
	Wait() (int, error)
	Pid() int
}

// Options configure a new Proc.
type Options struct {
	Shell string // empty = DefaultShell()
	Cols  int
	Rows  int
	Cwd   string // empty = inherit the daemon's working directory
}
