package fah

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hardening tests and fuzzers from the m1 chaos pass (docs/CHAOS_REPORT.md).
// They pass on the commit under test and pin behavior that held up.

// discardConn is a net.Conn whose writes succeed and are dropped. Only Write
// is used by wsConn.readMessage (to answer pings).
type discardConn struct{ net.Conn }

func (discardConn) Write(p []byte) (int, error) { return len(p), nil }

func FuzzReadMessage(f *testing.F) {
	f.Add(frame(true, opText, []byte(`{"config":{}}`)))
	f.Add(append(frame(false, opText, []byte("ab")), frame(true, opContinuation, []byte("cd"))...))
	f.Add(append(frame(true, opPing, []byte("p")), frame(true, opText, []byte("x"))...))
	f.Add([]byte{0x81, 127, 0, 0, 0, 0, 0, 0x10, 0, 0})
	f.Add([]byte{0x81, 126, 0xFF, 0xFF})
	f.Add(frame(true, opClose, []byte{0x03, 0xE8}))
	f.Fuzz(func(t *testing.T, in []byte) {
		w := &wsConn{conn: discardConn{}, br: bufio.NewReaderSize(bytes.NewReader(in), handshakeBufSize)}
		msg, err := w.readMessage()
		if err == nil && len(msg) > maxMessageBytes {
			t.Fatalf("message of %d bytes exceeds cap %d", len(msg), maxMessageBytes)
		}
		if err == nil && len(msg) > len(in) {
			t.Fatalf("message (%d bytes) larger than the input (%d bytes)", len(msg), len(in))
		}
	})
}

func FuzzReadHandshakeResponse(f *testing.F) {
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	f.Add([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + want + "\r\n\r\n"))
	f.Add([]byte("HTTP/1.1 101 X\nupgrade:WebSocket\nconnection: keep-alive, Upgrade\nsec-websocket-accept:" + want + "\n\n"))
	f.Add([]byte("HTTP/1.1 302 Found\r\nLocation: ws://10.0.0.1/\r\n\r\n"))
	f.Add([]byte("HTTP/1.1 101 OK\r\nSec-WebSocket-Protocol: x\r\n\r\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		r := bytes.NewReader(in)
		w := &wsConn{conn: discardConn{}, br: bufio.NewReaderSize(r, handshakeBufSize)}
		err := w.readHandshakeResponse(want)
		consumed := len(in) - r.Len() - w.br.Buffered()
		if err != nil {
			return
		}
		if consumed > maxHandshakeBytes {
			t.Fatalf("accepted a %d-byte handshake, cap is %d", consumed, maxHandshakeBytes)
		}
		head := strings.ToLower(string(in[:consumed]))
		if !strings.Contains(string(in[:consumed]), want) || !strings.Contains(head, "websocket") {
			t.Fatalf("accepted a handshake without the accept key or upgrade: %q", in[:consumed])
		}
		if !strings.HasPrefix(head, "http/1.") {
			t.Fatalf("accepted a non-HTTP/1 status line: %q", in[:consumed])
		}
	})
}

func FuzzValidateEndpoint(f *testing.F) {
	for _, s := range []string{
		DefaultEndpoint, "ws://[::1]:1/x", "ws://LOCALHOST", "ws://[::1%25lo0]/", "ws://[::ffff:127.0.0.1]/",
		"ws://2130706433/", "ws://0x7f.1/", "ws://017700000001/", "ws://localhost./", "ws://127.1/",
		"ws://127.0.0.1@10.0.0.1/", "ws://10.0.0.1#@127.0.0.1/", "ws://127.0.0.1\\@10.0.0.1/",
		"ws://ｌｏｃａｌｈｏｓｔ/", "ws://127.0.0.1:7396/a?b c", "ws://127.0.0.1:+80/", "ws://127.0.0.1:0x50/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, ep string) {
		dial, host, path, err := validateEndpoint(ep)
		if err != nil {
			return
		}
		h, port, splitErr := net.SplitHostPort(dial)
		if splitErr != nil {
			t.Fatalf("%q: dial address %q is not host:port: %v", ep, dial, splitErr)
		}
		if h != "127.0.0.1" && h != "::1" {
			t.Fatalf("%q: dials non-loopback host %q", ep, h)
		}
		if port == "" || strings.Trim(port, "0123456789") != "" {
			t.Fatalf("%q: dial port %q is not numeric", ep, port)
		}
		for _, v := range []string{host, path} {
			if strings.ContainsAny(v, "\r\n\x00") {
				t.Fatalf("%q: header or request URI %q contains CR, LF or NUL", ep, v)
			}
		}
		if !strings.HasPrefix(path, "/") {
			t.Fatalf("%q: request URI %q does not start with /", ep, path)
		}
	})
}

// Every accepted endpoint form must dial loopback; the obfuscated IPv4/IPv6
// spellings must be rejected outright.
func TestChaosEndpointSpellingsRejected(t *testing.T) {
	for _, ep := range []string{
		"ws://2130706433/", "ws://0x7f.1/", "ws://017700000001/", "ws://127.1/", "ws://0177.0.0.1/",
		"ws://localhost./", "ws://[::1%25lo0]/", "ws://[0:0:0:0:0:0:0:1]/", "ws://[::ffff:7f00:1]/",
		"ws://127.0.0.1@10.0.0.1/", "ws://10.0.0.1#@127.0.0.1/", "ws://127.0.0.1\\@10.0.0.1/",
		"ws://ｌｏｃａｌｈｏｓｔ/", "ws://%6cocalhost/", "ws:// 127.0.0.1/", "ws://127.0.0.1 /",
		"ws://127.0.0.1:+80/", "ws://127.0.0.1:0x50/", "ws://127.0.0.1:-1/",
		"ws://" + strings.Repeat("a", 1<<16) + "/",
	} {
		if dial, _, _, err := validateEndpoint(ep); err == nil {
			t.Errorf("validateEndpoint(%.60q) accepted, dials %q", ep, dial)
		}
	}
}

// The header-count guard counts the terminating blank line: 64 headers are
// accepted and 65 are rejected.
func TestChaosHandshakeHeaderLimitBoundary(t *testing.T) {
	for _, tc := range []struct {
		headers int
		ok      bool
	}{{maxHandshakeHeaders, true}, {maxHandshakeHeaders + 1, false}} {
		srv := startFakeFAH(t, func(sc *serverConn) {
			req, err := http.ReadRequest(sc.br)
			if err != nil {
				return
			}
			resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
				"Sec-WebSocket-Accept: " + acceptFor(req.Header.Get("Sec-WebSocket-Key")) + "\r\n" +
				strings.Repeat("X-Pad: 1\r\n", tc.headers-3) + "\r\n"
			_, _ = io.WriteString(sc.conn, resp)
			_ = sc.sendText(`{"config":{"paused":false},"units":[]}`)
			_, _, _ = sc.readFrame()
		})
		c, err := NewClientV8(srv.endpoint)
		if err != nil {
			t.Fatalf("NewClientV8: %v", err)
		}
		_, err = c.GetStatus(context.Background())
		if (err == nil) != tc.ok {
			t.Errorf("%d headers: err = %v, want ok=%t", tc.headers, err, tc.ok)
		}
		_ = c.Close()
	}
}

// A slowloris server that drips one byte every 20ms (always below any
// per-read timeout) must still be cut off by the overall ctx deadline.
func TestChaosSlowlorisBoundedByDeadline(t *testing.T) {
	stages := map[string]func(*serverConn){
		"handshake": func(sc *serverConn) {
			_, _ = http.ReadRequest(sc.br)
			for _, b := range []byte("HTTP/1.1 101 Switching Protocols\r\nX-Slow: " + strings.Repeat("z", 4000)) {
				if _, err := sc.conn.Write([]byte{b}); err != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		},
		"frame payload": func(sc *serverConn) {
			if sc.upgrade() != nil {
				return
			}
			_ = sc.send([]byte{0x81, 126, 0x10, 0x00})
			for range 4096 {
				if _, err := sc.conn.Write([]byte{' '}); err != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		},
		"ping drip": func(sc *serverConn) {
			if sc.upgrade() != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, sc.br) }()
			for range 1000 {
				if sc.send(frame(true, opPing, []byte("x"))) != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		},
	}
	for name, h := range stages {
		t.Run(name, func(t *testing.T) {
			srv := startFakeFAH(t, h)
			c, err := NewClientV8(srv.endpoint)
			if err != nil {
				t.Fatalf("NewClientV8: %v", err)
			}
			defer func() { _ = c.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err = c.GetStatus(ctx)
			if el := time.Since(start); el > 450*time.Millisecond {
				t.Fatalf("GetStatus took %v with a 300ms deadline", el)
			}
			// The error class (DeadlineExceeded vs a bare i/o timeout) is racy:
			// see TestChaosDeadlineErrorClassificationRace (chaos tag, CH-5).
			if err == nil {
				t.Fatal("GetStatus succeeded against a slowloris server")
			}
		})
	}
}

// An already-expired or already-canceled ctx must fail without dialing.
func TestChaosExpiredContext(t *testing.T) {
	var accepted atomic.Int32
	srv := startFakeFAH(t, func(sc *serverConn) { accepted.Add(1) })
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	defer func() { _ = c.Close() }()

	expired, cancel1 := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel1()
	canceled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	for _, ctx := range []context.Context{expired, canceled} {
		start := time.Now()
		if err := c.Pause(ctx); err == nil || ctx.Err() == nil || !errors.Is(err, ctx.Err()) {
			t.Errorf("Pause with dead ctx: err = %v", err)
		}
		if el := time.Since(start); el > 50*time.Millisecond {
			t.Errorf("Pause with dead ctx took %v", el)
		}
	}
	srv.ln.Close() //nolint:errcheck
	srv.wg.Wait()
	if n := accepted.Load(); n != 0 {
		t.Errorf("a dead ctx still opened %d connections", n)
	}
}

// Calls racing Close from many goroutines: no race, no panic, no leaked
// goroutine, and every call ends with nil or an error within its deadline.
func TestChaosCallsRacingClose(t *testing.T) {
	st := &fahState{project: 18626}
	srv := startFakeFAH(t, st.serve)
	base := runtime.NumGoroutine()
	for round := range 5 {
		c, err := NewClientV8(srv.endpoint)
		if err != nil {
			t.Fatalf("NewClientV8: %v", err)
		}
		var wg sync.WaitGroup
		for w := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				for i := range 5 {
					start := time.Now()
					switch (w + i) % 3 {
					case 0:
						_ = c.Pause(ctx)
					case 1:
						_ = c.Resume(ctx)
					default:
						if s, err := c.GetStatus(ctx); err != nil && s.State != StateUnknown {
							t.Errorf("error %v with state %q", err, s.State)
						}
					}
					if el := time.Since(start); el > 1100*time.Millisecond {
						t.Errorf("call took %v", el)
					}
				}
			}()
		}
		time.Sleep(time.Duration(round) * 3 * time.Millisecond)
		_ = c.Close()
		_ = c.Close()
		wg.Wait()
		if err := c.Resume(context.Background()); !errors.Is(err, ErrClosed) {
			t.Fatalf("Resume after Close = %v", err)
		}
		c.mu.Lock()
		n := len(c.conns)
		c.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d connections still tracked after all calls returned", n)
		}
	}
	waitForGoroutines(t, base)
}

// Worst-case memory per call against a server that sends the maximum number
// of maximum-size skipped messages: allocation must stay bounded by a small
// multiple of the 1 MiB cap.
func TestChaosWorstCaseAllocationIsBounded(t *testing.T) {
	big := bytes.Repeat([]byte{' '}, maxMessageBytes)
	big[0], big[len(big)-1] = '[', ']'
	// Pre-build the whole server stream so server-side allocations do not
	// pollute the measurement: maxSkippedMessages messages of exactly 1 MiB,
	// each split into the maximum number of fragments, then the snapshot.
	var stream []byte
	step := maxMessageBytes/(maxFramesPerMessage-1) + 1
	for range maxSkippedMessages {
		for off := 0; off < len(big); off += step {
			end := min(off+step, len(big))
			op := byte(opContinuation)
			if off == 0 {
				op = opText
			}
			stream = append(stream, frame(end == len(big), op, big[off:end])...)
		}
	}
	stream = append(stream, frame(true, opText, []byte(`{"config":{"paused":false},"units":[]}`))...)

	srv := startFakeFAH(t, func(sc *serverConn) {
		if sc.upgrade() != nil || sc.send(stream) != nil {
			return
		}
		_, _, _ = sc.readFrame()
	})
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	defer func() { _ = c.Close() }()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	st, err := c.GetStatus(context.Background())
	runtime.ReadMemStats(&after)
	if err != nil || st.State != StateIdle {
		t.Fatalf("GetStatus = %+v, %v", st, err)
	}
	total := after.TotalAlloc - before.TotalAlloc
	t.Logf("client allocated %.1f MiB for %d skipped 1 MiB messages in %d-byte fragments",
		float64(total)/(1<<20), maxSkippedMessages, step)
	if total > 32<<20 {
		t.Fatalf("allocated %d MiB, want < 32 MiB", total>>20)
	}
}

// Sanity check that the fuzz helper frame builder matches the parser for
// the 64-bit length form at the exact cap.
func TestChaosFrameAtExactCap(t *testing.T) {
	payload := bytes.Repeat([]byte{' '}, maxMessageBytes)
	hdr := []byte{0x81, 127}
	hdr = binary.BigEndian.AppendUint64(hdr, uint64(len(payload)))
	w := &wsConn{conn: discardConn{}, br: bufio.NewReader(io.MultiReader(bytes.NewReader(hdr), bytes.NewReader(payload)))}
	msg, err := w.readMessage()
	if err != nil || len(msg) != maxMessageBytes {
		t.Fatalf("exact-cap frame: len=%d err=%v", len(msg), err)
	}
	hdr = binary.BigEndian.AppendUint64([]byte{0x81, 127}, uint64(len(payload)+1))
	w = &wsConn{conn: discardConn{}, br: bufio.NewReader(io.MultiReader(bytes.NewReader(hdr), bytes.NewReader(append(payload, ' '))))}
	if _, err := w.readMessage(); !errors.Is(err, errMessageLarge) {
		t.Fatalf("cap+1 frame: err=%v, want errMessageLarge", err)
	}
}

// Control frames interleaved between the fragments of the snapshot (allowed
// by RFC 6455 section 5.4) must be answered and must not corrupt the message.
func TestChaosInterleavedControlFramesInSnapshot(t *testing.T) {
	snap := []byte(`{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":18626}}]}`)
	var stream []byte
	for i := range snap {
		op := byte(opContinuation)
		if i == 0 {
			op = opText
		}
		stream = append(stream, frame(true, opPing, []byte{byte(i)})...)
		stream = append(stream, frame(true, opPong, nil)...)
		stream = append(stream, frame(i == len(snap)-1, op, snap[i:i+1])...)
	}
	pongs := make(chan int, 1)
	srv := startFakeFAH(t, func(sc *serverConn) {
		if sc.upgrade() != nil || sc.send(stream) != nil {
			return
		}
		n := 0
		for {
			op, _, err := sc.readFrame()
			if err != nil || op == opClose {
				pongs <- n
				_ = sc.send(frame(true, opClose, nil))
				return
			}
			if op == opPong {
				n++
			}
		}
	})
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	defer func() { _ = c.Close() }()
	st, err := c.GetStatus(context.Background())
	if err != nil || st != (Status{ActiveProjectID: 18626, State: StateFolding}) {
		t.Fatalf("GetStatus = %+v, %v", st, err)
	}
	if n := <-pongs; n != len(snap) {
		t.Fatalf("client answered %d pings, want %d", n, len(snap))
	}
}
