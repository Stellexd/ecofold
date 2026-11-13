// Regression tests for the second chaos pass (docs/CHAOS_REPORT_2.md).

package fah

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// CH2-1: a ClientV8 not built by NewClientV8 must not panic: Close is a no-op
// and calls fail with an error.
func TestZeroValueClientV8DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("zero-value ClientV8 panicked: %v", r)
		}
	}()
	var c ClientV8
	if _, err := c.GetStatus(context.Background()); err == nil {
		t.Fatal("GetStatus on a zero-value ClientV8 succeeded")
	}
	if err := c.Pause(context.Background()); err == nil {
		t.Fatal("Pause on a zero-value ClientV8 succeeded")
	}
	_ = c.Close()
}

// CH2-4: one Pause against a FAH client that never reports paused must back
// off between state reads instead of opening a connection every 50 ms.
func TestUnconfirmedPauseBacksOff(t *testing.T) {
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
