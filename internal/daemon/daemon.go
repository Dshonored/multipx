// Package daemon implements multipxd: the long-lived process that owns PTY
// sessions and serves them to clients over QUIC. Sessions outlive any one
// client connection, which is the entire point — closing your terminal, or
// losing the network, never kills the shell underneath it.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"

	"multipx/internal/proto"
	"multipx/internal/session"
	"multipx/internal/transport"
)

// DefaultAddr is loopback-only out of the box. Binding to a non-loopback
// address is fine on a trusted LAN now that connections are gated by a
// shared token (see token.go) instead of just an open port — see
// transport.ServerTLSConfig's TODO for the remaining TLS-trust caveat.
const DefaultAddr = "127.0.0.1:7890"

type Daemon struct {
	ID      string
	Addr    string
	Manager *session.Manager

	// Token gates every incoming connection (see authenticate). Left empty,
	// Run loads/generates the machine's persisted token via EnsureToken;
	// set it explicitly before Run to bypass that (tests do this to avoid
	// touching the real user's token file).
	Token string
}

func New(addr string) *Daemon {
	if addr == "" {
		addr = DefaultAddr
	}
	return &Daemon{
		ID:      newID(),
		Addr:    addr,
		Manager: session.NewManager(),
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Run listens and serves until ctx is cancelled — the usual entry point
// for a standalone multipxd process. Callers that need to know the bind
// succeeded before moving on (the Wails app embeds a daemon and can't let
// its own first connection attempt race the listener coming up) should
// call Listen and Serve directly instead.
func (d *Daemon) Run(ctx context.Context) error {
	ln, err := d.Listen()
	if err != nil {
		return err
	}
	return d.Serve(ctx, ln)
}

// Listen loads/generates the auth token (if not already set) and binds
// the QUIC listener. Separated from Serve so a caller can confirm the
// bind succeeded — and so a bind failure because something else is
// already listening (another multipx-app instance, a standalone multipxd
// the user started by hand) can be told apart from every other startup
// error, without having to race the accept loop to find out.
func (d *Daemon) Listen() (*quic.Listener, error) {
	if d.Token == "" {
		tok, err := EnsureToken()
		if err != nil {
			return nil, fmt.Errorf("load auth token: %w", err)
		}
		d.Token = tok
	}

	tlsConf, err := transport.ServerTLSConfig()
	if err != nil {
		return nil, err
	}
	ln, err := quic.ListenAddr(d.Addr, tlsConf, transport.QUICConfig())
	if err != nil {
		return nil, err
	}
	log.Printf("multipxd listening on %s (id=%s)", ln.Addr(), d.ID)
	return ln, nil
}

// Serve runs the accept loop until ctx is cancelled, closing ln either
// way.
func (d *Daemon) Serve(ctx context.Context, ln *quic.Listener) error {
	defer ln.Close()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.handleConn(ctx, conn)
	}
}

func (d *Daemon) handleConn(ctx context.Context, conn *quic.Conn) {
	ctrl, err := conn.AcceptStream(ctx)
	if err != nil {
		return
	}
	if !d.authenticate(ctrl) {
		conn.CloseWithError(0, "unauthorized")
		return
	}
	go d.handleControl(ctx, conn, ctrl)

	for {
		s, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go d.handleAttach(s)
	}
}

// authenticate consumes the control stream's first frame, which must be a
// Hello carrying the shared token. Nothing else is served on this
// connection — including attach streams, which are only ever opened on a
// connection that already has an authenticated control stream — until
// this succeeds, so a client can't skip straight past Hello to open an
// attach stream without ever proving it knows the token.
func (d *Daemon) authenticate(ctrl *quic.Stream) bool {
	env, err := proto.Recv(ctrl)
	if err != nil || env.Type != proto.MsgHello {
		return false
	}
	var m proto.HelloMsg
	_ = env.Decode(&m)
	if m.Token != d.Token {
		_ = proto.Send(ctrl, proto.MsgError, proto.ErrorMsg{Message: "unauthorized: invalid token"})
		return false
	}
	hostname, _ := os.Hostname()
	return proto.Send(ctrl, proto.MsgHelloAck, proto.HelloAckMsg{
		ProtocolVersion: proto.ProtocolVersion,
		DaemonID:        d.ID,
		Hostname:        hostname,
	}) == nil
}

func (d *Daemon) handleControl(ctx context.Context, conn *quic.Conn, ctrl *quic.Stream) {
	for {
		env, err := proto.Recv(ctrl)
		if err != nil {
			return
		}
		switch env.Type {
		case proto.MsgListSessions:
			_ = proto.Send(ctrl, proto.MsgSessionList, proto.SessionListMsg{
				Sessions: d.Manager.List(),
			})

		case proto.MsgCreateSession:
			var m proto.CreateSessionMsg
			_ = env.Decode(&m)
			s, err := d.Manager.Create(m.Name, m.Shell, m.Cols, m.Rows)
			if err != nil {
				_ = proto.Send(ctrl, proto.MsgError, proto.ErrorMsg{Message: err.Error()})
				continue
			}
			_ = proto.Send(ctrl, proto.MsgSessionCreated, proto.SessionCreatedMsg{Session: s.Info()})

		case proto.MsgKillSession:
			var m proto.KillSessionMsg
			_ = env.Decode(&m)
			if s, ok := d.Manager.Get(m.SessionID); ok {
				_ = s.Kill()
				d.Manager.Remove(s.ID)
			}

		case proto.MsgResize:
			var m proto.ResizeMsg
			_ = env.Decode(&m)
			if s, ok := d.Manager.Get(m.SessionID); ok {
				_ = s.Resize(m.Cols, m.Rows)
			}

		case proto.MsgRenameSession:
			var m proto.RenameSessionMsg
			_ = env.Decode(&m)
			if s, ok := d.Manager.Get(m.SessionID); ok {
				if _, err := d.Manager.Rename(s.ID, m.Name); err != nil {
					_ = proto.Send(ctrl, proto.MsgError, proto.ErrorMsg{Message: err.Error()})
				}
			}

		case proto.MsgGetUsage:
			_ = proto.Send(ctrl, proto.MsgUsage, currentUsage())

		default:
			_ = proto.Send(ctrl, proto.MsgError, proto.ErrorMsg{Message: "unknown message type: " + string(env.Type)})
		}
	}
}

// handleAttach claims a freshly opened stream for one session: a single
// framed Attach handshake, then raw PTY bytes in both directions until the
// client closes the stream (detach) or the connection drops.
func (d *Daemon) handleAttach(stream *quic.Stream) {
	env, err := proto.Recv(stream)
	if err != nil || env.Type != proto.MsgAttach {
		return
	}
	var m proto.AttachMsg
	_ = env.Decode(&m)

	s, ok := d.Manager.Get(m.SessionID)
	if !ok {
		_ = proto.Send(stream, proto.MsgError, proto.ErrorMsg{Message: "no such session: " + m.SessionID})
		return
	}
	if m.Cols > 0 && m.Rows > 0 {
		_ = s.Resize(m.Cols, m.Rows)
	}

	scrollback := s.Scrollback()
	if err := proto.Send(stream, proto.MsgAttachOK, proto.AttachOKMsg{
		SessionID:     s.ID,
		ScrollbackLen: len(scrollback),
	}); err != nil {
		return
	}
	if len(scrollback) > 0 {
		if _, err := stream.Write(scrollback); err != nil {
			return
		}
	}

	attachID := newID()
	s.Attach(attachID, stream)
	defer s.Detach(attachID)

	buf := make([]byte, 8*1024)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			if werr := s.Input(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("attach stream read: %v", err)
			}
			return
		}
	}
}

// currentUsage samples CPU/memory for the machine the daemon is running
// on. The CPU reading blocks briefly (it has to sample over an interval to
// mean anything) — acceptable here since it's one request on one client's
// control stream, not something on the hot path.
func currentUsage() proto.UsageMsg {
	hostname, _ := os.Hostname()
	u := proto.UsageMsg{Hostname: hostname}

	if pct, err := cpu.Percent(200*time.Millisecond, false); err == nil && len(pct) > 0 {
		u.CPUPercent = pct[0]
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		u.MemUsedBytes = vm.Used
		u.MemTotal = vm.Total
	}
	return u
}
