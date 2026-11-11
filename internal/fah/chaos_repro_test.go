//go:build chaos

// Chaos repro tests that still FAIL or are unverified on this platform. See
// docs/CHAOS_REPORT.md. Fixed findings live in regression_test.go.
// Run with: go test -tags chaos -race -run Chaos -v ./internal/fah
package fah

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// CH-2 (Linux only, HYPOTHESIS on macOS): Close does not abort a call that is still dialing. Close only closes
// connections that were already tracked, so a dial that hangs (the listener's
// accept queue is full and SYNs are dropped) keeps running until the caller's
// deadline, contradicting "Close() aborts in-flight calls".
func TestChaosCloseDoesNotAbortInFlightDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Never Accept: fill the accept queue so further SYNs are dropped.
	var fillers []net.Conn
	defer func() {
		for _, f := range fillers {
			_ = f.Close()
		}
	}()
	hangs := false
	for range 1024 {
		f, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
		if err != nil {
			var ne net.Error
			hangs = errors.As(err, &ne) && ne.Timeout()
			break
		}
		fillers = append(fillers, f)
	}
	if !hangs {
		// macOS answers a full accept queue with RST, so the dial fails fast
		// and the window cannot be held open. Linux (tcp_abort_on_overflow=0,
		// the default) drops the SYN and the dial hangs.
		t.Skip("this OS resets instead of dropping SYNs on a full accept queue; run on Linux")
	}

	c, err := NewClientV8("ws://" + ln.Addr().String() + "/api/websocket")
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	time.AfterFunc(50*time.Millisecond, func() { _ = c.Close() })
	start := time.Now()
	_, err = c.GetStatus(ctx)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("GetStatus took %v after Close at 50ms (err=%v): Close does not abort an in-flight dial", elapsed, err)
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("GetStatus error = %v, want ErrClosed", err)
	}
}
