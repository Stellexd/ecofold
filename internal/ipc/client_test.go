package ipc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestClientHonorsContext checks a deadline and a cancellation both unblock
// a Call whose handler never answers.
func TestClientHonorsContext(t *testing.T) {
	h := &echoHandler{block: make(chan struct{})}
	defer close(h.block)
	ts := startServer(t, h, nil)

	c, err := Dial(context.Background(), ts.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Call(ctx, CmdForcePause, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, se esperaba DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Call tardó %v con un deadline de 150 ms", d)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel2)
	start = time.Now()
	if _, err := c.Call(ctx2, CmdForcePause, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, se esperaba Canceled", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Call tardó %v tras cancelar", d)
	}

	// The client recovers on a fresh connection.
	if _, err := c.Call(context.Background(), CmdStatus, nil); err != nil {
		t.Fatalf("Call tras el timeout: %v", err)
	}
}

// TestClientRedialsAfterIdleClose checks a long-lived client survives the
// server's idle timeout.
func TestClientRedialsAfterIdleClose(t *testing.T) {
	ts := startServer(t, &echoHandler{}, func(s *Server) { s.ioTimeout = 100 * time.Millisecond })

	c, err := Dial(context.Background(), ts.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	for i := 0; i < 3; i++ {
		if _, err := c.Call(context.Background(), CmdStatus, nil); err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestDialWithoutDaemon checks the error a GUI sees when the daemon is down.
func TestDialWithoutDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Dial(ctx, filepath.Join(shortDir(t), "missing.sock")); err == nil {
		t.Fatal("Dial conectó sin daemon")
	}
}
