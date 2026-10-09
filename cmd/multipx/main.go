// Command multipx is the client: it talks to a running multipxd daemon to
// list, create, and attach to sessions.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"multipx/internal/client"
	"multipx/internal/daemon"
	"multipx/internal/tui"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "multipx:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	addr := daemon.DefaultAddr
	if v := os.Getenv("MULTIPX_ADDR"); v != "" {
		addr = v
	}
	token := os.Getenv("MULTIPX_TOKEN")
	if token == "" {
		// No override: fall back to this machine's own token, which is
		// correct for the common case (addr is the local daemon) and a
		// no-op cost otherwise — a remote daemon will just reject it,
		// same as an empty token would.
		if t, err := daemon.EnsureToken(); err == nil {
			token = t
		}
	}
	ctx := context.Background()

	if len(args) == 0 {
		return cmdTUI(ctx, addr, token)
	}

	switch args[0] {
	case "tui":
		return cmdTUI(ctx, addr, token)
	case "ls":
		return cmdList(ctx, addr, token)
	case "new":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		return cmdNew(ctx, addr, token, name)
	case "attach":
		if len(args) < 2 {
			return fmt.Errorf("usage: multipx attach <name>")
		}
		return cmdAttach(ctx, addr, token, args[1])
	default:
		return usage()
	}
}

func usage() error {
	fmt.Fprintln(os.Stderr, `usage:
  multipx                    open the session multiplexer UI
  multipx tui                same as above, explicitly
  multipx ls                 list sessions
  multipx new [name]         create a session and attach to it (no UI)
  multipx attach <name>      attach to an existing session (no UI)

env:
  MULTIPX_ADDR   daemon address (default 127.0.0.1:7890)
  MULTIPX_TOKEN  daemon auth token (default: this machine's own token —
                 only needed to reach a remote daemon; fetch its token
                 over SSH with "multipxd -print-token")`)
	return nil
}

func cmdTUI(ctx context.Context, addr, token string) error {
	c, err := client.Dial(ctx, addr, token)
	if err != nil {
		return err
	}
	m := tui.New(ctx, c)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	final, err := p.Run()
	if fm, ok := final.(tui.Model); ok {
		fm.Close()
	} else {
		_ = c.Close()
	}
	return err
}

func cmdList(ctx context.Context, addr, token string) error {
	c, err := client.Dial(ctx, addr, token)
	if err != nil {
		return err
	}
	defer c.Close()

	sessions, err := c.List()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no sessions")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tSHELL\tATTACHED\tLAST ACTIVE")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			s.Name, s.Status, s.Shell, s.Attached, humanAge(s.LastActive))
	}
	return tw.Flush()
}

func humanAge(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm ago", int(d.Minutes()))
}

func cmdNew(ctx context.Context, addr, token, name string) error {
	c, err := client.Dial(ctx, addr, token)
	if err != nil {
		return err
	}
	defer c.Close()

	cols, rows := termSize()
	s, err := c.Create(name, "", cols, rows)
	if err != nil {
		return err
	}
	return attachLoop(ctx, c, s.ID, s.Name)
}

func cmdAttach(ctx context.Context, addr, token, name string) error {
	c, err := client.Dial(ctx, addr, token)
	if err != nil {
		return err
	}
	defer c.Close()
	return attachLoop(ctx, c, name, name)
}

func termSize() (cols, rows int) {
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		return w, h
	}
	return 80, 24
}

func attachLoop(ctx context.Context, c *client.Client, sessionID, label string) error {
	cols, rows := termSize()
	stream, ok, err := c.AttachStream(ctx, sessionID, cols, rows)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "attached to %s (replayed %d bytes) — Ctrl-B D to detach\r\n", label, ok.ScrollbackLen)

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("stdin is not a terminal: %w", err)
	}
	defer term.Restore(fd, oldState)

	var once sync.Once
	done := make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }

	go func() {
		_, _ = io.Copy(os.Stdout, stream)
		finish()
	}()
	go func() {
		pumpInput(stream)
		finish()
	}()

	<-done
	_ = stream.Close()
	fmt.Fprint(os.Stderr, "\r\ndetached\r\n")
	return nil
}

// pumpInput forwards stdin to w, intercepting the tmux-style Ctrl-B prefix:
// Ctrl-B D detaches, anything else after Ctrl-B is forwarded verbatim
// (including the prefix byte), which is a deliberate v1 simplification —
// a literal Ctrl-B cannot be sent through to the remote shell yet.
func pumpInput(w io.Writer) {
	buf := make([]byte, 1)
	prefixArmed := false
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			b := buf[0]
			switch {
			case prefixArmed && (b == 'd' || b == 'D'):
				return
			case prefixArmed:
				prefixArmed = false
				_, _ = w.Write([]byte{0x02, b})
			case b == 0x02: // Ctrl-B
				prefixArmed = true
			default:
				_, _ = w.Write([]byte{b})
			}
		}
		if err != nil {
			return
		}
	}
}
