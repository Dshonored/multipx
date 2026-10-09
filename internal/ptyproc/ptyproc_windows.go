//go:build windows

package ptyproc

import (
	"context"
	"os"

	"github.com/UserExistsError/conpty"
)

// DefaultShell returns the interactive shell to launch when none is
// requested: PowerShell if present, falling back to COMSPEC (cmd.exe).
func DefaultShell() string {
	if _, err := os.Stat(os.ExpandEnv(`${SystemRoot}\System32\WindowsPowerShell\v1.0\powershell.exe`)); err == nil {
		return "powershell.exe"
	}
	if comspec := os.Getenv("COMSPEC"); comspec != "" {
		return comspec
	}
	return "cmd.exe"
}

type winProc struct {
	cpty *conpty.ConPty
}

func Start(opts Options) (Proc, error) {
	shell := opts.Shell
	if shell == "" {
		shell = DefaultShell()
	}
	cols, rows := opts.Cols, opts.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	copts := []conpty.ConPtyOption{
		conpty.ConPtyDimensions(cols, rows),
		conpty.ConPtyEnv(append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")),
	}
	if opts.Cwd != "" {
		copts = append(copts, conpty.ConPtyWorkDir(opts.Cwd))
	}
	cpty, err := conpty.Start(shell, copts...)
	if err != nil {
		return nil, err
	}
	return &winProc{cpty: cpty}, nil
}

func (p *winProc) Read(b []byte) (int, error)  { return p.cpty.Read(b) }
func (p *winProc) Write(b []byte) (int, error) { return p.cpty.Write(b) }
func (p *winProc) Resize(cols, rows int) error { return p.cpty.Resize(cols, rows) }
func (p *winProc) Close() error                { return p.cpty.Close() }
func (p *winProc) Pid() int                    { return p.cpty.Pid() }

func (p *winProc) Wait() (int, error) {
	code, err := p.cpty.Wait(context.Background())
	return int(code), err
}
