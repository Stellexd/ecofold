//go:build chaos

package fah

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// CH2-1: a ClientV8 not built by NewClientV8 used to tolerate Close (nil map
// range is fine). Close now calls the nil shutdown func and panics.
func TestChaos2ZeroValueClosePanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Close on zero-value ClientV8 panicked: %v", r)
		}
	}()
	var c ClientV8
	_ = c.Close()
}

// CH2-4: one Pause against a FAH client that never reports paused opens a new
// TCP connection + WebSocket handshake every ~50 ms until the deadline.
func TestChaos2PauseConnectionAmplification(t *testing.T) {
	var conns atomic.Int64
	srv := startFakeFAH(t, func(sc *serverConn) {
		conns.Add(1)
		st := &fahState{project: 18626}
		if sc.upgrade() != nil || sc.sendText(st.snapshot()) != nil {
			return
		}
		// Well-behaved peer that ignores the state command.
		for {
			op, payload, err := sc.readFrame()
			if err != nil {
				return
			}
			if op == opClose {
				_ = sc.send(frame(true, opClose, payload))
				return
			}
		}
	})
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	// Uses the default 3 s timeout, as the daemon would.
	start := time.Now()
	err = c.Pause(context.Background())
	t.Logf("Pause err=%v after %v, connections=%d", err, time.Since(start), conns.Load())
	if n := conns.Load(); n > 10 {
		t.Fatalf("a single unconfirmed Pause opened %d connections to FAH", n)
	}
}
