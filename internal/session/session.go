package session

import (
	"io"
	"maps"
	"sync"
	"time"

	"multipx/internal/proto"
	"multipx/internal/ptyproc"
)

// scrollbackCap bounds how much output we replay to a reattaching client.
const scrollbackCap = 256 * 1024

// Session owns one PTY-backed shell and fans its output out to every
// currently-attached client, independent of whether any client is attached
// at all — the shell keeps running either way, which is the whole point.
type Session struct {
	ID        string
	Name      string
	Shell     string
	CreatedAt time.Time

	proc ptyproc.Proc

	mu         sync.Mutex
	scrollback *ringBuffer
	writers    map[string]io.Writer
	lastActive time.Time
	exited     bool
	exitCode   int
}

func newSession(id, name, shell string, proc ptyproc.Proc) *Session {
	now := time.Now()
	s := &Session{
		ID:         id,
		Name:       name,
		Shell:      shell,
		CreatedAt:  now,
		proc:       proc,
		scrollback: newRingBuffer(scrollbackCap),
		writers:    make(map[string]io.Writer),
		lastActive: now,
	}
	go s.pump()
	return s
}

// pump reads PTY output until the process exits, writing every chunk to the
// scrollback buffer and broadcasting it live to attached writers.
func (s *Session) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.proc.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.scrollback.Write(chunk)
			s.broadcast(chunk)
			s.touch()
		}
		if err != nil {
			code, _ := s.proc.Wait()
			s.mu.Lock()
			s.exited = true
			s.exitCode = code
			s.mu.Unlock()
			return
		}
	}
}

func (s *Session) broadcast(data []byte) {
	s.mu.Lock()
	writers := make(map[string]io.Writer, len(s.writers))
	maps.Copy(writers, s.writers)
	s.mu.Unlock()

	for k, w := range writers {
		if _, err := w.Write(data); err != nil {
			s.Detach(k)
		}
	}
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// Scrollback returns everything currently buffered, for replay on attach.
func (s *Session) Scrollback() []byte {
	return s.scrollback.Bytes()
}

// Attach registers w to receive live output and returns an attachment id to
// pass to Detach later.
func (s *Session) Attach(attachID string, w io.Writer) {
	s.mu.Lock()
	s.writers[attachID] = w
	s.mu.Unlock()
}

func (s *Session) Detach(attachID string) {
	s.mu.Lock()
	delete(s.writers, attachID)
	s.mu.Unlock()
}

// GetName returns the session's current display name.
func (s *Session) GetName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Name
}

// setName applies a new display name. Unexported: callers go through
// Manager.Rename, which holds the registry lock for the uniqueness check.
func (s *Session) setName(name string) {
	s.mu.Lock()
	s.Name = name
	s.mu.Unlock()
}

// Input forwards keystrokes/paste data from an attached client to the PTY.
func (s *Session) Input(p []byte) error {
	_, err := s.proc.Write(p)
	if err == nil {
		s.touch()
	}
	return err
}

func (s *Session) Resize(cols, rows int) error {
	return s.proc.Resize(cols, rows)
}

func (s *Session) Kill() error {
	return s.proc.Close()
}

func (s *Session) Info() proto.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := proto.StatusRunning
	if s.exited {
		status = proto.StatusExited
	}
	return proto.SessionInfo{
		ID:         s.ID,
		Name:       s.Name,
		Shell:      s.Shell,
		Status:     status,
		ExitCode:   s.exitCode,
		CreatedAt:  s.CreatedAt,
		LastActive: s.lastActive,
		Attached:   len(s.writers),
	}
}
