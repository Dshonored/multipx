package session

import "sync"

// ringBuffer keeps the most recent N bytes written to it, so a client that
// reattaches to a session can replay what it missed instead of opening onto
// a blank screen.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{cap: capacity}
}

func (r *ringBuffer) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.cap; over > 0 {
		r.buf = append([]byte(nil), r.buf[over:]...)
	}
}

// Bytes returns a copy of the buffered content, oldest first.
func (r *ringBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}
