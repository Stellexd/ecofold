package fah

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Many calls must not leak goroutines from the per-call AfterFunc on c.done.
func TestChaos2NoAfterFuncLeak(t *testing.T) {
	st := &fahState{project: 18626}
	srv := startFakeFAH(t, st.serve)
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	base := runtime.NumGoroutine()
	for range 500 {
		if _, err := c.GetStatus(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	waitForGoroutines(t, base)
	_ = c.Close()
	_ = c.Close()
	if _, err := c.GetStatus(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
	if err := c.Pause(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Pause after Close: %v", err)
	}
}

// Close racing with new calls: every call ends with nil or ErrClosed, never a
// panic or a hang.
func TestChaos2CloseRacesCalls(t *testing.T) {
	st := &fahState{project: 18626}
	srv := startFakeFAH(t, st.serve)
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := c.GetStatus(ctx)
			if err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	time.Sleep(2 * time.Millisecond)
	_ = c.Close()
	wg.Wait()
}

// Errors from an unconfirmed Pause wrap the ctx cause; a cancel wins over a
// deadline that never came.
func TestChaos2ConfirmErrorsWrapCtx(t *testing.T) {
	never := func(sc *serverConn) {
		st := &fahState{project: 1}
		if sc.upgrade() != nil || sc.sendText(st.snapshot()) != nil {
			return
		}
		for {
			if _, _, err := sc.readFrame(); err != nil {
				return
			}
		}
	}
	srv := startFakeFAH(t, never)
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := c.Pause(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(120*time.Millisecond, cancel2)
	if err := c.Pause(ctx2); !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: %v", err)
	}
}

// A dial that fails immediately (closed port) is not misreported as a ctx error.
func TestChaos2DialRefusedNotDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c, err := NewClientV8("ws://" + addr + "/api/websocket")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.GetStatus(context.Background())
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrClosed) {
		t.Fatalf("refused dial: %v", err)
	}
}

// CH-4 follow-up: legitimate endpoints still accepted; raw control bytes rejected.
func TestChaos2EndpointCharset(t *testing.T) {
	for _, ok := range []string{
		"ws://127.0.0.1:7396/api/websocket?a=b&c=%20d",
		"ws://[::1]:7396/api/websocket",
		"ws://LOCALHOST/api/websocket?x=" + strings.Repeat("a", 4096),
		"ws://127.0.0.1/a%0Ab", // percent-encoded LF stays encoded on the wire
	} {
		if _, err := NewClientV8(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"ws://127.0.0.1/api?a=b c",
		"ws://127.0.0.1/api?a=\tb",
		"ws://127.0.0.1/api?a=\x7f",
	} {
		if _, err := NewClientV8(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Numeric edge cases of the tolerant project parser and the status invariants.
func TestChaos2ProjectIDEdges(t *testing.T) {
	cases := map[string]int{
		`18626`: 18626, `18626.0`: 18626, `"18626"`: 18626, `1e5`: 100000, `1E4`: 10000,
		`18626.0000000001`: 0, `-0`: 0, `0`: 0, `2147483647`: 2147483647, `2147483648`: 0,
		`"0x48C2"`: 0, `" 18626"`: 0, `"+18626"`: 0, `1e400`: 0, `null`: 0, `true`: 0,
		`123456789012345678901234567890`: 0, `{}`: 0,
	}
	for in, want := range cases {
		msg := `{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":` + in + `}}]}`
		st, err := parseFAHMessage([]byte(msg))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if st.ActiveProjectID != want {
			t.Errorf("%s: project %d, want %d", in, st.ActiveProjectID, want)
		}
		if (st.ActiveProjectID > 0) != (st.State == StateFolding) {
			t.Errorf("%s: invariant broken: %+v", in, st)
		}
	}
}

// FuzzChaos2ParseInvariants checks Paused <=> StatePaused and
// ActiveProjectID>0 <=> StateFolding on any accepted snapshot.
func FuzzChaos2ParseInvariants(f *testing.F) {
	f.Add(`{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":18626}}]}`)
	f.Add(`{"config":{"paused":true},"units":[{"state":"RUN","assignment":{"project":"1e3"}}]}`)
	f.Add(`{"config":{"paused":false},"units":[1,{"state":"RUN","pause_reason":"","assignment":{"project":7.0}}]}`)
	f.Fuzz(func(t *testing.T, s string) {
		st, err := parseFAHMessage([]byte(s))
		if err != nil {
			if st.State != StateUnknown {
				t.Fatalf("error with state %v", st.State)
			}
			return
		}
		if st.Paused != (st.State == StatePaused) {
			t.Fatalf("paused invariant: %+v", st)
		}
		if (st.ActiveProjectID > 0) != (st.State == StateFolding) || st.ActiveProjectID < 0 {
			t.Fatalf("folding invariant: %+v", st)
		}
	})
}
