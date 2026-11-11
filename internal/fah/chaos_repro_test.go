//go:build chaos

// Chaos repro tests. Every test in this file FAILS on the commit under test
// and documents a bug found by the m1 chaos pass (docs/CHAOS_REPORT.md).
// Run with: go test -tags chaos -race -run Chaos -v ./internal/fah
package fah

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// CH-1: Pause reports success although the FAH client never received the
// command. The server sends a valid snapshot and then goes away (FAH crash or
// restart). The client's write lands in the local socket buffer, the error of
// the graceful close is swallowed, and Pause returns nil, so a daemon would
// believe folding is paused while it is not.
func TestChaosPauseReportsSuccessWithoutDelivery(t *testing.T) {
	srv := startFakeFAH(t, func(sc *serverConn) {
		if sc.upgrade() != nil {
			return
		}
		_ = sc.sendText(`{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":18626}}]}`)
		// The peer dies right after the snapshot: no frame is ever read, so
		// the pause command cannot have been delivered.
		_ = sc.conn.Close()
	})
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Pause(context.Background()); err == nil {
		t.Fatal("Pause returned nil, but the server closed the connection without ever reading the command (no acknowledgement is verified)")
	}
}

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

// CH-3: Pause is gated on decoding the whole snapshot, including fields it
// does not need. One unit whose project is not a JSON integer (a string, or
// a float such as 18626.0) makes json.Unmarshal fail, readStatus fails, and
// the pause command is never sent: folding keeps running during the game
// (fail-open) for as long as that unit is in the snapshot.
func TestChaosPauseBlockedByUnitSchemaDrift(t *testing.T) {
	for _, snap := range []string{
		`{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":18626.0}}]}`,
		`{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":"18626"}}]}`,
		`{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":18626}},{"state":"DOWNLOAD","assignment":[]}]}`,
	} {
		t.Run(snap[len(snap)-40:], func(t *testing.T) {
			var mu sync.Mutex
			var got []string
			srv := startFakeFAH(t, func(sc *serverConn) {
				if sc.upgrade() != nil || sc.sendText(snap) != nil {
					return
				}
				for {
					op, payload, err := sc.readFrame()
					if err != nil || op == opClose {
						_ = sc.send(frame(true, opClose, nil))
						return
					}
					mu.Lock()
					got = append(got, string(payload))
					mu.Unlock()
				}
			})
			c, err := NewClientV8(srv.endpoint)
			if err != nil {
				t.Fatalf("NewClientV8: %v", err)
			}
			defer func() { _ = c.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			if err := c.Pause(ctx); err != nil {
				t.Fatalf("Pause failed against a live FAH client because of an unrelated unit field: %v", err)
			}
		})
	}
}

// CH-4: the query string is copied verbatim into the HTTP request line. A
// space in the configured endpoint (which net/url accepts in the query) adds
// extra tokens to the request line, so the client emits a malformed request
// such as "GET /api/websocket?x HTTP/1.0 HTTP/1.1".
func TestChaosQuerySpaceBreaksRequestLine(t *testing.T) {
	lines := make(chan string, 1)
	srv := startFakeFAH(t, func(sc *serverConn) {
		line, _ := sc.br.ReadString('\n')
		lines <- strings.TrimRight(line, "\r\n")
	})
	ep := strings.Replace(srv.endpoint, "/api/websocket", "/api/websocket?x HTTP/1.0", 1)
	c, err := NewClientV8(ep)
	if err != nil {
		t.Skipf("endpoint rejected (fixed): %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _ = c.GetStatus(ctx)

	line := <-lines
	if parts := strings.Split(line, " "); len(parts) != 3 {
		t.Fatalf("request line %q has %d space-separated tokens, want 3 (method, URI, version)", line, len(parts))
	}
}

// CH-5: when the ctx deadline expires during a read, the error is sometimes a
// bare "i/o timeout" instead of wrapping context.DeadlineExceeded. The conn
// deadline and the ctx timer fire at the same instant; do() classifies the
// error with ctx.Err(), which can still be nil when the netpoller wins. The
// existing TestClientV8DefaultTimeout is flaky for the same reason (about
// 13-18% of runs fail locally with -count=300).
func TestChaosDeadlineErrorClassificationRace(t *testing.T) {
	srv := startFakeFAH(t, func(sc *serverConn) {
		if sc.upgrade() != nil {
			return
		}
		go func() { _, _ = io.Copy(io.Discard, sc.br) }()
		for range 2000 {
			if sc.send(frame(true, opPing, nil)) != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	defer func() { _ = c.Close() }()

	bad := 0
	const runs = 40
	var last error
	for range runs {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := c.GetStatus(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			bad++
			last = err
		}
	}
	if bad > 0 {
		t.Fatalf("%d/%d calls that hit their ctx deadline did not wrap context.DeadlineExceeded; last: %v", bad, runs, last)
	}
}
