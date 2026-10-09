//go:build !windows

package ptyproc

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// DefaultShell returns the user's login shell, falling back to /bin/sh.
func DefaultShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

type unixProc struct {
	cmd *exec.Cmd
	f   *os.File
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

	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &unixProc{cmd: cmd, f: f}, nil
}

func (p *unixProc) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *unixProc) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *unixProc) Resize(cols, rows int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *unixProc) Close() error {
	_ = p.cmd.Process.Kill()
	return p.f.Close()
}

func (p *unixProc) Pid() int {
	if p.cmd.Process == nil {
		return -1
	}
	return p.cmd.Process.Pid
}

func (p *unixProc) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}
