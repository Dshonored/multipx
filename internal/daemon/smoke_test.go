package daemon_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"multipx/internal/client"
	"multipx/internal/daemon"
)

// TestDetachReattachReplaysScrollback proves the core claim of the whole
// project: a session's output keeps flowing while nobody is attached, and a
// client that reattaches sees what it missed instead of a blank screen.
func TestDetachReattachReplaysScrollback(t *testing.T) {
	const addr = "127.0.0.1:17890"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := daemon.New(addr)
	d.Token = "smoke-test-token" // explicit, so this never touches the real user's token file
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond) // let the listener come up

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	c, err := client.Dial(dialCtx, addr, d.Token)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	sess, err := c.Create("smoke", "", 80, 24)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created session %s (%s)", sess.Name, sess.ID)

	attachCtx, attachCancel := context.WithTimeout(ctx, 5*time.Second)
	defer attachCancel()
	stream, ok, err := c.AttachStream(attachCtx, sess.ID, 80, 24)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if ok.ScrollbackLen != 0 {
		t.Fatalf("expected empty scrollback on first attach, got %d bytes", ok.ScrollbackLen)
	}

	const marker = "hello-multipx-smoke-test"
	if _, err := stream.Write([]byte("echo " + marker + "\r\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	if !waitForMarker(t, stream, marker, 5*time.Second) {
		t.Fatalf("marker %q never appeared in live output", marker)
	}

	// Detach: close our side of the stream. The shell and its output buffer
	// keep going on the daemon without us.
	_ = stream.Close()
	time.Sleep(300 * time.Millisecond)

	reattachCtx, reattachCancel := context.WithTimeout(ctx, 5*time.Second)
	defer reattachCancel()
	stream2, ok2, err := c.AttachStream(reattachCtx, sess.ID, 80, 24)
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if ok2.ScrollbackLen == 0 {
		t.Fatalf("expected non-empty scrollback on reattach")
	}
	t.Logf("reattach replayed %d bytes of scrollback", ok2.ScrollbackLen)

	replay := make([]byte, ok2.ScrollbackLen)
	if _, err := readFull(stream2, replay); err != nil {
		t.Fatalf("read replay: %v", err)
	}
	if !strings.Contains(string(replay), marker) {
		t.Fatalf("replayed scrollback does not contain marker %q:\n%s", marker, replay)
	}

	_ = stream2.Close()
	cancel()
	<-runErr
}

func waitForMarker(t *testing.T, r interface{ Read([]byte) (int, error) }, marker string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var buf bytes.Buffer
	chunk := make([]byte, 4096)
	for time.Now().Before(deadline) {
		type result struct {
			n   int
			err error
		}
		rc := make(chan result, 1)
		go func() {
			n, err := r.Read(chunk)
			rc <- result{n, err}
		}()
		select {
		case res := <-rc:
			if res.n > 0 {
				buf.Write(chunk[:res.n])
				if strings.Contains(buf.String(), marker) {
					return true
				}
			}
			if res.err != nil {
				return false
			}
		case <-time.After(timeout):
			return false
		}
	}
	return false
}

func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
