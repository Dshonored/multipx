// Package proto defines the wire protocol between multipxd (daemon) and
// multipx (client) over a QUIC connection.
//
// One QUIC connection per client. Stream 0 (the first stream the client
// opens) is the control stream: length-prefixed JSON envelopes for
// discovery and session lifecycle (hello, list, create, attach, resize,
// detach). Attaching to a session opens a NEW stream; the client sends a
// single framed Attach envelope as a handshake, the server replies with
// AttachOK followed by the scrollback replay, and from then on the stream
// carries raw PTY bytes in both directions with no further framing — it is
// just a duplex pipe between the client's terminal and the session's PTY.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ProtocolVersion is bumped on any wire-incompatible change.
const ProtocolVersion = 1

const maxFrameSize = 16 << 20 // 16MiB safety cap on a single control frame

type MsgType string

const (
	MsgHello          MsgType = "hello"
	MsgHelloAck       MsgType = "hello_ack"
	MsgListSessions   MsgType = "list_sessions"
	MsgSessionList    MsgType = "session_list"
	MsgCreateSession  MsgType = "create_session"
	MsgSessionCreated MsgType = "session_created"
	MsgAttach         MsgType = "attach"
	MsgAttachOK       MsgType = "attach_ok"
	MsgResize         MsgType = "resize"
	MsgDetach         MsgType = "detach"
	MsgKillSession    MsgType = "kill_session"
	MsgRenameSession  MsgType = "rename_session"
	MsgGetUsage       MsgType = "get_usage"
	MsgUsage          MsgType = "usage"
	MsgError          MsgType = "error"
)

// Envelope wraps every control-stream message so the reader can dispatch on
// Type before decoding Payload into the concrete struct.
type Envelope struct {
	Type    MsgType         `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type HelloMsg struct {
	ProtocolVersion int    `json:"protocol_version"`
	ClientName      string `json:"client_name"`
	Token           string `json:"token"`
}

type HelloAckMsg struct {
	ProtocolVersion int    `json:"protocol_version"`
	DaemonID        string `json:"daemon_id"`
	Hostname        string `json:"hostname"`
}

type SessionStatus string

const (
	StatusRunning SessionStatus = "running"
	StatusExited  SessionStatus = "exited"
)

type SessionInfo struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Shell      string        `json:"shell"`
	Status     SessionStatus `json:"status"`
	ExitCode   int           `json:"exit_code,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	LastActive time.Time     `json:"last_active"`
	Attached   int           `json:"attached"` // number of live attachments
}

type ListSessionsMsg struct{}

type SessionListMsg struct {
	Sessions []SessionInfo `json:"sessions"`
}

type CreateSessionMsg struct {
	Name  string `json:"name,omitempty"`
	Shell string `json:"shell,omitempty"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
}

type SessionCreatedMsg struct {
	Session SessionInfo `json:"session"`
}

// AttachMsg is sent as the sole handshake frame on a freshly opened stream
// to claim it for a given session.
type AttachMsg struct {
	SessionID string `json:"session_id"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

type AttachOKMsg struct {
	SessionID     string `json:"session_id"`
	ScrollbackLen int    `json:"scrollback_len"`
}

type ResizeMsg struct {
	SessionID string `json:"session_id"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

type DetachMsg struct {
	SessionID string `json:"session_id"`
}

type KillSessionMsg struct {
	SessionID string `json:"session_id"`
}

type RenameSessionMsg struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

type GetUsageMsg struct{}

// UsageMsg reports the daemon host's current resource usage, so a client
// (local or remote) can show what it's actually connected to, not just
// whether it's reachable.
type UsageMsg struct {
	Hostname     string  `json:"hostname"`
	CPUPercent   float64 `json:"cpu_percent"`
	MemUsedBytes uint64  `json:"mem_used_bytes"`
	MemTotal     uint64  `json:"mem_total_bytes"`
}

type ErrorMsg struct {
	Message string `json:"message"`
}

// WriteFrame writes a length-prefixed JSON-encoded value.
func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFrame reads a length-prefixed JSON-encoded value into v.
func ReadFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameSize {
		return fmt.Errorf("proto: frame too large (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// Send marshals payload and writes it as an Envelope of the given type.
func Send(w io.Writer, t MsgType, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return WriteFrame(w, Envelope{Type: t, Payload: b})
}

// Recv reads the next Envelope off r.
func Recv(r io.Reader) (Envelope, error) {
	var env Envelope
	err := ReadFrame(r, &env)
	return env, err
}

// Decode unmarshals an envelope's payload into v.
func (e Envelope) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}
