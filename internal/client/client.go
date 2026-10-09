// Package client is the daemon-facing half of multipx: dial a daemon over
// QUIC, issue control-stream requests, and open attach streams.
package client

import (
	"context"
	"fmt"
	"sync"

	"github.com/quic-go/quic-go"

	"multipx/internal/proto"
	"multipx/internal/transport"
)

// Client is safe for concurrent use: the control stream is request/response
// and shared, so every call that touches it takes ctrlMu for the full
// round trip. AttachStream opens its own stream and never touches the
// control stream, so attached sessions don't contend with it.
type Client struct {
	conn  *quic.Conn
	ctrl  *quic.Stream
	token string

	ctrlMu sync.Mutex
}

// Dial connects to a daemon at addr and completes the hello handshake,
// authenticating with token (the daemon's shared secret — see
// daemon.EnsureToken and "multipxd -print-token" for how to obtain one for
// a remote machine). The daemon rejects the connection outright if token
// doesn't match.
func Dial(ctx context.Context, addr, token string) (*Client, error) {
	conn, err := quic.DialAddr(ctx, addr, transport.ClientTLSConfig(), transport.QUICConfig())
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	ctrl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, ctrl: ctrl, token: token}
	if _, err := c.Hello(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() error {
	return c.conn.CloseWithError(0, "bye")
}

func (c *Client) Hello() (proto.HelloAckMsg, error) {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	if err := proto.Send(c.ctrl, proto.MsgHello, proto.HelloMsg{
		ProtocolVersion: proto.ProtocolVersion,
		ClientName:      "multipx-cli",
		Token:           c.token,
	}); err != nil {
		return proto.HelloAckMsg{}, err
	}
	env, err := proto.Recv(c.ctrl)
	if err != nil {
		return proto.HelloAckMsg{}, err
	}
	if env.Type == proto.MsgError {
		return proto.HelloAckMsg{}, decodeErr(env)
	}
	var ack proto.HelloAckMsg
	_ = env.Decode(&ack)
	return ack, nil
}

func (c *Client) List() ([]proto.SessionInfo, error) {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	if err := proto.Send(c.ctrl, proto.MsgListSessions, proto.ListSessionsMsg{}); err != nil {
		return nil, err
	}
	env, err := proto.Recv(c.ctrl)
	if err != nil {
		return nil, err
	}
	if env.Type == proto.MsgError {
		return nil, decodeErr(env)
	}
	var list proto.SessionListMsg
	_ = env.Decode(&list)
	return list.Sessions, nil
}

// GetUsage asks the daemon for its host machine's current CPU/memory
// usage — a snapshot of what the daemon is actually running on, not just
// whether it's reachable.
func (c *Client) GetUsage() (proto.UsageMsg, error) {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	if err := proto.Send(c.ctrl, proto.MsgGetUsage, proto.GetUsageMsg{}); err != nil {
		return proto.UsageMsg{}, err
	}
	env, err := proto.Recv(c.ctrl)
	if err != nil {
		return proto.UsageMsg{}, err
	}
	if env.Type == proto.MsgError {
		return proto.UsageMsg{}, decodeErr(env)
	}
	var u proto.UsageMsg
	_ = env.Decode(&u)
	return u, nil
}

func (c *Client) Create(name, shell string, cols, rows int) (proto.SessionInfo, error) {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	if err := proto.Send(c.ctrl, proto.MsgCreateSession, proto.CreateSessionMsg{
		Name: name, Shell: shell, Cols: cols, Rows: rows,
	}); err != nil {
		return proto.SessionInfo{}, err
	}
	env, err := proto.Recv(c.ctrl)
	if err != nil {
		return proto.SessionInfo{}, err
	}
	if env.Type == proto.MsgError {
		return proto.SessionInfo{}, decodeErr(env)
	}
	var created proto.SessionCreatedMsg
	_ = env.Decode(&created)
	return created.Session, nil
}

// Resize notifies the daemon that an attached session's terminal size
// changed. Fire-and-forget: the daemon sends no reply.
func (c *Client) Resize(sessionID string, cols, rows int) error {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	return proto.Send(c.ctrl, proto.MsgResize, proto.ResizeMsg{
		SessionID: sessionID, Cols: cols, Rows: rows,
	})
}

// Rename changes a session's display name. Fire-and-forget: the daemon
// sends no reply on success, only on failure.
func (c *Client) Rename(sessionID, name string) error {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	return proto.Send(c.ctrl, proto.MsgRenameSession, proto.RenameSessionMsg{
		SessionID: sessionID, Name: name,
	})
}

// Kill terminates a session's process and removes it from the daemon's
// registry. Fire-and-forget: the daemon sends no reply.
func (c *Client) Kill(sessionID string) error {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	return proto.Send(c.ctrl, proto.MsgKillSession, proto.KillSessionMsg{SessionID: sessionID})
}

// AttachStream opens a new stream and claims it for sessionID. On success
// the returned stream is positioned right after the handshake: scrollback
// (if any) arrives as the first bytes of normal stream output, so the
// caller can go straight into raw bidirectional copying.
func (c *Client) AttachStream(ctx context.Context, sessionID string, cols, rows int) (*quic.Stream, proto.AttachOKMsg, error) {
	stream, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, proto.AttachOKMsg{}, err
	}
	if err := proto.Send(stream, proto.MsgAttach, proto.AttachMsg{
		SessionID: sessionID, Cols: cols, Rows: rows,
	}); err != nil {
		return nil, proto.AttachOKMsg{}, err
	}
	env, err := proto.Recv(stream)
	if err != nil {
		return nil, proto.AttachOKMsg{}, err
	}
	if env.Type == proto.MsgError {
		return nil, proto.AttachOKMsg{}, decodeErr(env)
	}
	var ok proto.AttachOKMsg
	_ = env.Decode(&ok)
	return stream, ok, nil
}

func decodeErr(env proto.Envelope) error {
	var e proto.ErrorMsg
	_ = env.Decode(&e)
	if e.Message == "" {
		e.Message = "unknown error"
	}
	return fmt.Errorf("%s", e.Message)
}
