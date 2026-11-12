package fah

// Regression tests for the findings of the m1 chaos pass (docs/CHAOS_REPORT.md).
// Each one failed before its fix.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// CH-1: Pause must not report success when FAH never applied the command. The
// server sends a valid snapshot and dies before reading the command.
func TestPauseFailsWithoutDelivery(t *testing.T) {
	srv := startFakeFAH(t, func(sc *serverConn) {
		if sc.upgrade() != nil {
			return
		}
		_ = sc.sendText(`{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":18626}}]}`)
		_ = sc.conn.Close()
	})
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if err := c.Pause(ctx); err == nil {
		t.Fatal("Pause returned nil, but the server never read the command")
	}
}

// CH-3: units with unexpected field types must not block a pause. The fake
// server applies commands, so Pause can be confirmed.
func TestPauseToleratesUnitSchemaDrift(t *testing.T) {
	for _, units := range []string{
		`[{"state":"RUN","assignment":{"project":18626.0}}]`,
		`[{"state":"RUN","assignment":{"project":"18626"}}]`,
		`[{"state":"RUN","assignment":{"project":18626}},{"state":"DOWNLOAD","assignment":[]}]`,
		`[{"state":42},{"state":"RUN","assignment":{"project":18626}}]`,
	} {
		t.Run(units, func(t *testing.T) {
			var mu sync.Mutex
			paused := false
			srv := startFakeFAH(t, func(sc *serverConn) {
				if sc.upgrade() != nil {
					return
				}
				mu.Lock()
				snap := fmt.Sprintf(`{"groups":{"":{"config":{"paused":%t}}},"units":%s}`, paused, units)
				mu.Unlock()
				if sc.sendText(snap) != nil {
					return
				}
				for {
					op, payload, err := sc.readFrame()
					if err != nil || op == opClose {
						_ = sc.send(frame(true, opClose, nil))
						return
					}
					if strings.Contains(string(payload), `"pause"`) {
						mu.Lock()
						paused = true
						mu.Unlock()
					}
				}
			})
			c, err := NewClientV8(srv.endpoint)
			if err != nil {
				t.Fatalf("NewClientV8: %v", err)
			}
			defer func() { _ = c.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			if err := c.Pause(ctx); err != nil {
				t.Fatalf("Pause: %v", err)
			}
		})
	}
}

func TestParseFAHMessageToleratesUnitSchemaDrift(t *testing.T) {
	for _, tc := range []struct {
		units string
		want  Status
	}{
		{`[{"state":"RUN","assignment":{"project":18626.0}}]`, Status{ActiveProjectID: 18626, State: StateFolding}},
		{`[{"state":"RUN","assignment":{"project":"18626"}}]`, Status{ActiveProjectID: 18626, State: StateFolding}},
		{`[{"state":"RUN","assignment":[]},{"state":"RUN","assignment":{"project":7}}]`, Status{ActiveProjectID: 7, State: StateFolding}},
		{`[{"state":"RUN","assignment":{"project":1e999}}]`, Status{State: StateIdle}},
		{`[{"state":"RUN","assignment":{"project":18626.5}}]`, Status{State: StateIdle}},
		{`[{"state":"RUN","assignment":{"project":"abc"}}]`, Status{State: StateIdle}},
		{`[{"state":"RUN","assignment":{"project":-3}}]`, Status{State: StateIdle}},
		{`[null, 5, "x", {"state":"RUN","assignment":{"project":9}}]`, Status{ActiveProjectID: 9, State: StateFolding}},
	} {
		msg := `{"config":{"paused":false},"units":` + tc.units + `}`
		got, err := parseFAHMessage([]byte(msg))
		if err != nil || got != tc.want {
			t.Errorf("parseFAHMessage(%s) = %+v, %v; want %+v", tc.units, got, err, tc.want)
		}
	}
}

// CH-4: spaces or control characters in the endpoint would corrupt the HTTP
// request line.
func TestNewClientV8RejectsSpacesInRequestURI(t *testing.T) {
	for _, ep := range []string{
		"ws://127.0.0.1:7396/api/websocket?x HTTP/1.0",
		"ws://127.0.0.1:7396/api/websocket?a=\tb",
		"ws://127.0.0.1:7396/api/websocket?a=\x7f",
	} {
		if _, err := NewClientV8(ep); err == nil {
			t.Errorf("NewClientV8(%q) accepted an endpoint that breaks the request line", ep)
		}
	}
	if _, err := NewClientV8("ws://127.0.0.1:7396/api/websocket?a=b%20c"); err != nil {
		t.Errorf("percent-encoded query rejected: %v", err)
	}
}

// CH-5: a call that hits its ctx deadline must always wrap
// context.DeadlineExceeded, even when the I/O timeout fires first.
func TestDeadlineErrorAlwaysWrapsDeadlineExceeded(t *testing.T) {
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

	for i := range 40 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := c.GetStatus(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("call %d: error %v does not wrap context.DeadlineExceeded", i, err)
		}
	}
}

// Close during the confirmation loop must end Pause promptly with ErrClosed
// instead of polling until the deadline. The server never applies commands.
func TestCloseStopsPauseConfirmation(t *testing.T) {
	srv := startFakeFAH(t, func(sc *serverConn) {
		if sc.upgrade() != nil || sc.sendText(`{"config":{"paused":false},"units":[]}`) != nil {
			return
		}
		_, _ = io.Copy(io.Discard, sc.br)
	})
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	time.AfterFunc(150*time.Millisecond, func() { _ = c.Close() })
	start := time.Now()
	err = c.Pause(ctx)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Pause error = %v, want ErrClosed", err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("Pause took %v after Close at 150ms", el)
	}
}

// CH-2: Close must abort a call that is still dialing. The dialer blocks
// until its ctx is done, as a SYN to a listener that drops it would.
func TestCloseAbortsInFlightDial(t *testing.T) {
	c, err := NewClientV8("")
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	dialing := make(chan struct{})
	c.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(dialing)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() {
		<-dialing
		_ = c.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err = c.GetStatus(ctx)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("GetStatus error = %v, want ErrClosed", err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("GetStatus took %v: Close did not abort the dial", el)
	}
}
