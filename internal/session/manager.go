package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

	"multipx/internal/proto"
	"multipx/internal/ptyproc"
)

// Manager is the daemon's registry of every session it owns, running or
// exited, regardless of whether a client is currently attached.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

func NewManager() *Manager {
	return &Manager{sessions: make(map[string]*Session)}
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Create spawns a new PTY-backed session. If name collides with an existing
// one, a short suffix is appended so names stay usable as handles without
// the caller having to pre-check.
func (m *Manager) Create(name, shell string, cols, rows int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := newID()
	for name == "" {
		name = "session-" + id
	}
	if _, taken := m.findByName(name); taken {
		name = fmt.Sprintf("%s-%s", name, id[:4])
	}

	proc, err := ptyproc.Start(ptyproc.Options{Shell: shell, Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	s := newSession(id, name, shell, proc)
	m.sessions[id] = s
	return s, nil
}

func (m *Manager) findByName(name string) (*Session, bool) {
	for _, s := range m.sessions {
		if s.GetName() == name {
			return s, true
		}
	}
	return nil, false
}

// Rename changes a session's display name, appending a short suffix on
// collision exactly like Create does, so a rename can never fail outright
// or silently merge two sessions under one name.
func (m *Manager) Rename(id, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return "", fmt.Errorf("no such session: %s", id)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return s.GetName(), nil
	}
	if other, taken := m.findByName(name); taken && other != s {
		name = fmt.Sprintf("%s-%s", name, id[:4])
	}
	s.setName(name)
	return name, nil
}

// Get resolves a session by ID or, failing that, by name.
func (m *Manager) Get(idOrName string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[idOrName]; ok {
		return s, true
	}
	return m.findByName(idOrName)
}

// List returns every session in creation order, oldest first, so a
// sidebar's row order stays stable across polls instead of jumping around
// with Go's randomized map iteration.
func (m *Manager) List() []proto.SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]proto.SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}
