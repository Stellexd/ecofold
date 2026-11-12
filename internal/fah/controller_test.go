package fah

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Fake FAH v8 server -----------------------------------------------------

// fakeFAH is a loopback TCP server that speaks just enough WebSocket to stand
// in for a FAH v8 client. Each connection is handled by handler.
type fakeFAH struct {
	ln       net.Listener
	endpoint string
	wg       sync.WaitGroup
}

func startFakeFAH(t *testing.T, handler func(*serverConn)) *fakeFAH {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeFAH{ln: ln, endpoint: "ws://" + ln.Addr().String() + "/api/websocket"}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer conn.Close()
				// Safety net so a buggy test cannot hang the suite.
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				handler(&serverConn{conn: conn, br: bufio.NewReader(conn)})
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

type serverConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// upgrade reads the client handshake and answers 101 Switching Protocols.
func (s *serverConn) upgrade() error {
	req, err := http.ReadRequest(s.br)
	if err != nil {
		return err
	}
	if req.URL.Path != "/api/websocket" {
		return fmt.Errorf("unexpected path %q", req.URL.Path)
	}
	sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + websocketGUID))
	_, err = io.WriteString(s.conn, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	return err
}

// frame builds an unmasked server frame.
func frame(fin bool, op byte, payload []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	buf := []byte{b0}
	switch n := len(payload); {
	case n <= 125:
		buf = append(buf, byte(n))
	case n <= 0xFFFF:
		buf = append(buf, 126)
		buf = binary.BigEndian.AppendUint16(buf, uint16(n))
	default:
		buf = append(buf, 127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	return append(buf, payload...)
}

func (s *serverConn) send(b []byte) error {
	_, err := s.conn.Write(b)
	return err
}

func (s *serverConn) sendText(msg string) error {
	return s.send(frame(true, opText, []byte(msg)))
}

// readFrame reads one masked client frame.
func (s *serverConn) readFrame() (op byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(s.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	if hdr[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame not masked")
	}
	n := uint64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(s.br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(s.br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if _, err = io.ReadFull(s.br, mask[:]); err != nil {
		return 0, nil, err
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(s.br, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return hdr[0] & 0x0F, payload, nil
}

// fahState is the shared state of the happy-path fake client.
type fahState struct {
	mu       sync.Mutex
	paused   bool
	project  int
	commands []stateCommand
}

func (st *fahState) snapshot() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	pauseReason := ""
	if st.paused {
		pauseReason = "Paused by user"
	}
	return fmt.Sprintf(`{"info":{"version":"8.5.6"},`+
		`"groups":{"":{"config":{"paused":%t}}},`+
		`"units":[{"id":"u1","state":"RUN","pause_reason":%q,"assignment":{"project":%d}}]}`,
		st.paused, pauseReason, st.project)
}

// serve behaves like FAH v8: snapshot first, then apply state commands until
// the client closes.
func (st *fahState) serve(sc *serverConn) {
	if sc.upgrade() != nil || sc.sendText(st.snapshot()) != nil {
		return
	}
	for {
		op, payload, err := sc.readFrame()
		if err != nil {
			return
		}
		switch op {
		case opClose:
			_ = sc.send(frame(true, opClose, payload))
			return
		case opText:
			var cmd stateCommand
			if json.Unmarshal(payload, &cmd) != nil || cmd.Cmd != "state" {
				continue
			}
			st.mu.Lock()
			st.commands = append(st.commands, cmd)
			switch cmd.State {
			case "pause":
				st.paused = true
			case "fold":
				st.paused = false
			}
			st.mu.Unlock()
			// Real clients stream incremental updates as arrays.
			_ = sc.sendText(fmt.Sprintf(`["groups","","config","paused",%t]`, cmd.State == "pause"))
		}
	}
}

// waitForGoroutines fails the test if the goroutine count does not return to
// base, which would indicate a leaked reader or watchdog.
func waitForGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutines leaked: %d > %d\n%s", runtime.NumGoroutine(), base, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- (a) Happy path -----------------------------------------------------------

func TestClientV8PauseResumeStatus(t *testing.T) {
	st := &fahState{project: 18626}
	srv := startFakeFAH(t, st.serve)
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	defer c.Close()
	ctx := context.Background()

	got, err := c.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if want := (Status{ActiveProjectID: 18626, State: StateFolding}); got != want {
		t.Fatalf("GetStatus = %+v, want %+v", got, want)
	}

	if err := c.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got, err = c.GetStatus(ctx); err != nil || got != (Status{Paused: true, State: StatePaused}) {
		t.Fatalf("after Pause: %+v, %v", got, err)
	}

	if err := c.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, err = c.GetStatus(ctx); err != nil || got.State != StateFolding || got.ActiveProjectID != 18626 {
		t.Fatalf("after Resume: %+v, %v", got, err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.commands) != 2 || st.commands[0].State != "pause" || st.commands[1].State != "fold" {
		t.Fatalf("server received %+v, want pause then fold", st.commands)
	}
	for _, cmd := range st.commands {
		if _, err := time.Parse(time.RFC3339, cmd.Time); err != nil || !strings.HasSuffix(cmd.Time, "Z") {
			t.Errorf("command time %q is not an ISO 8601 UTC timestamp", cmd.Time)
		}
	}
}

// TestClientV8Concurrent runs many concurrent calls in two phases. Pause and
// Resume confirm the resulting state, so within a phase every mutating call
// asks for the same state; mixing them would make the confirmations race each
// other by design (the daemon issues one command at a time).
func TestClientV8Concurrent(t *testing.T) {
	st := &fahState{project: 18626}
	srv := startFakeFAH(t, st.serve)
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	defer c.Close()

	for _, phase := range []struct {
		name   string
		mutate func(context.Context) error
		want   string
	}{
		{"pause", c.Pause, StatePaused},
		{"resume", c.Resume, StateFolding},
	} {
		const workers, iterations = 6, 4
		var wg sync.WaitGroup
		errs := make(chan error, workers*iterations)
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				for i := range iterations {
					var err error
					if (w+i)%2 == 0 {
						err = phase.mutate(ctx)
					} else {
						var s Status
						s, err = c.GetStatus(ctx)
						if err == nil && s.State != StatePaused && s.State != StateFolding {
							err = fmt.Errorf("unexpected state %q", s.State)
						}
					}
					if err != nil {
						errs <- fmt.Errorf("%s phase: %w", phase.name, err)
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if s, err := c.GetStatus(context.Background()); err != nil || s.State != phase.want {
			t.Fatalf("after %s phase: %+v, %v; want %s", phase.name, s, err, phase.want)
		}
	}
}

// --- (b) Loopback-only endpoints -----------------------------------------------

func TestNewClientV8RejectsNonLoopback(t *testing.T) {
	for _, ep := range []string{
		"ws://192.168.1.50:7396/api/websocket",
		"ws://10.0.0.1:7396/api/websocket",
		"ws://0.0.0.0:7396/api/websocket",
		"ws://127.0.0.2:7396/api/websocket", // loopback range, but not allowed
		"ws://[::ffff:127.0.0.1]:7396/api/websocket",
		"ws://example.com:7396/api/websocket",
		"ws://localhost.evil.example:7396/api/websocket",
		"ws://127.0.0.1.nip.io:7396/api/websocket",
		"ws://user:pass@127.0.0.1:7396/api/websocket",
		"wss://127.0.0.1:7396/api/websocket",
		"http://127.0.0.1:7396/api/websocket",
		"ws://127.0.0.1:0/api/websocket",
		"ws://127.0.0.1:99999/api/websocket",
		"ws://127.0.0.1:7396/api/websocket#frag",
		"ws:127.0.0.1",
		"ws://127.0.0.1:7396/api\r\nX-Injected: 1",
	} {
		if _, err := NewClientV8(ep); err == nil {
			t.Errorf("NewClientV8(%q) accepted a non-loopback or invalid endpoint", ep)
		}
	}
}

func TestNewClientV8AcceptsLoopback(t *testing.T) {
	for _, tc := range []struct{ endpoint, dial, host, path string }{
		{"", "127.0.0.1:7396", "127.0.0.1:7396", "/api/websocket"},
		{"ws://127.0.0.1:7396/api/websocket", "127.0.0.1:7396", "127.0.0.1:7396", "/api/websocket"},
		{"ws://[::1]:7396/api/websocket", "[::1]:7396", "[::1]:7396", "/api/websocket"},
		// localhost is pinned to 127.0.0.1 instead of being resolved.
		{"ws://LocalHost:8000/api/websocket", "127.0.0.1:8000", "LocalHost:8000", "/api/websocket"},
		{"ws://127.0.0.1", "127.0.0.1:7396", "127.0.0.1:7396", "/api/websocket"},
	} {
		c, err := NewClientV8(tc.endpoint)
		if err != nil {
			t.Errorf("NewClientV8(%q): %v", tc.endpoint, err)
			continue
		}
		if c.dialAddr != tc.dial || c.host != tc.host || c.path != tc.path {
			t.Errorf("NewClientV8(%q) = dial %q host %q path %q, want %q %q %q",
				tc.endpoint, c.dialAddr, c.host, c.path, tc.dial, tc.host, tc.path)
		}
	}
}

// --- (c) Hanging servers ------------------------------------------------------

func TestClientV8HangingServerCancels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(*serverConn)
	}{
		{"no handshake response", func(sc *serverConn) { _, _ = io.Copy(io.Discard, sc.br) }},
		{"no snapshot", func(sc *serverConn) {
			if sc.upgrade() == nil {
				_, _ = io.Copy(io.Discard, sc.br)
			}
		}},
		{"partial frame", func(sc *serverConn) {
			if sc.upgrade() == nil && sc.send([]byte{0x81, 0x7E, 0x10}) == nil {
				_, _ = io.Copy(io.Discard, sc.br)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFakeFAH(t, tc.handler)
			c, err := NewClientV8(srv.endpoint)
			if err != nil {
				t.Fatalf("NewClientV8: %v", err)
			}
			base := runtime.NumGoroutine()

			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(50*time.Millisecond, cancel)
			start := time.Now()
			err = c.Pause(ctx)
			elapsed := time.Since(start)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Pause error = %v, want context.Canceled", err)
			}
			if elapsed > 250*time.Millisecond { // 50 ms until cancel + < 200 ms to react
				t.Fatalf("Pause took %v after a 50ms cancel", elapsed)
			}
			waitForGoroutines(t, base)
		})
	}
}

func TestClientV8DefaultTimeout(t *testing.T) {
	srv := startFakeFAH(t, func(sc *serverConn) { _, _ = io.Copy(io.Discard, sc.br) })
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	if c.timeout != 3*time.Second {
		t.Fatalf("default timeout = %v, want 3s", c.timeout)
	}
	c.timeout = 100 * time.Millisecond // shorten the default for the test

	start := time.Now()
	_, err = c.GetStatus(context.Background()) // no deadline from the caller
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetStatus error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("GetStatus took %v with a 100ms default timeout", elapsed)
	}
}

func TestClientV8CloseAbortsInFlight(t *testing.T) {
	srv := startFakeFAH(t, func(sc *serverConn) { _, _ = io.Copy(io.Discard, sc.br) })
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}

	time.AfterFunc(50*time.Millisecond, func() { _ = c.Close() })
	if _, err := c.GetStatus(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("in-flight GetStatus error = %v, want ErrClosed", err)
	}
	if err := c.Resume(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Resume after Close = %v, want ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// --- (d) Hostile servers --------------------------------------------------------

func TestClientV8HostileServers(t *testing.T) {
	upgraded := func(then func(*serverConn)) func(*serverConn) {
		return func(sc *serverConn) {
			if sc.upgrade() == nil {
				then(sc)
			}
		}
	}
	chunk := bytes.Repeat([]byte("a"), 512<<10)

	for _, tc := range []struct {
		name    string
		handler func(*serverConn)
	}{
		{"binary garbage instead of HTTP", func(sc *serverConn) {
			_ = sc.send([]byte{0x00, 0xFF, 0xFE, 0x13, 0x37, '\n', 0x80, 0x81})
		}},
		{"HTTP 200 instead of 101", func(sc *serverConn) {
			_, _ = http.ReadRequest(sc.br)
			_ = sc.send([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
		}},
		{"redirect", func(sc *serverConn) {
			_, _ = http.ReadRequest(sc.br)
			_ = sc.send([]byte("HTTP/1.1 302 Found\r\nLocation: ws://192.168.1.50:7396/\r\n\r\n"))
		}},
		{"wrong accept key", func(sc *serverConn) {
			_, _ = http.ReadRequest(sc.br)
			_ = sc.send([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n" +
				"Connection: Upgrade\r\nSec-WebSocket-Accept: bm90LXRoZS1rZXk=\r\n\r\n"))
		}},
		{"endless header line", func(sc *serverConn) {
			_, _ = http.ReadRequest(sc.br)
			_ = sc.send([]byte("HTTP/1.1 101 Switching Protocols\r\nX-Junk: "))
			_ = sc.send(bytes.Repeat([]byte("x"), 64<<10))
		}},
		{"too many headers", func(sc *serverConn) {
			_, _ = http.ReadRequest(sc.br)
			_ = sc.send([]byte("HTTP/1.1 101 Switching Protocols\r\n"))
			_ = sc.send(bytes.Repeat([]byte("X-A: b\r\n"), 1000))
		}},
		{"binary frame", upgraded(func(sc *serverConn) {
			_ = sc.send(frame(true, opBinary, []byte{0xDE, 0xAD, 0xBE, 0xEF}))
		})},
		{"truncated JSON", upgraded(func(sc *serverConn) {
			_ = sc.sendText(`{"groups":{"":{"config":{"paused":fal`)
		})},
		{"JSON with wrong types", upgraded(func(sc *serverConn) {
			_ = sc.sendText(`{"groups":{"":{"config":{"paused":"yes"}}},"units":"none"}`)
		})},
		{"object without config or groups", upgraded(func(sc *serverConn) {
			_ = sc.sendText(`{"hello":"world"}`)
		})},
		{"only updates, never a snapshot", upgraded(func(sc *serverConn) {
			for range 10 {
				_ = sc.sendText(`["units",0,"progress",0.5]`)
			}
		})},
		{"invalid UTF-8 text", upgraded(func(sc *serverConn) {
			_ = sc.send(frame(true, opText, []byte{0xFF, 0xFE, 0xFD}))
		})},
		{"frame header claims 2 MiB", upgraded(func(sc *serverConn) {
			hdr := []byte{0x81, 127}
			_ = sc.send(binary.BigEndian.AppendUint64(hdr, 2<<20))
		})},
		{"frame header claims 2^63 bytes", upgraded(func(sc *serverConn) {
			_ = sc.send([]byte{0x81, 127, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
		})},
		{"fragments exceed 1 MiB", upgraded(func(sc *serverConn) {
			_ = sc.send(frame(false, opText, chunk))
			_ = sc.send(frame(false, opContinuation, chunk))
			_ = sc.send(frame(true, opContinuation, chunk))
		})},
		{"endless empty fragments", upgraded(func(sc *serverConn) {
			_ = sc.send(frame(false, opText, nil))
			_ = sc.send(bytes.Repeat(frame(false, opContinuation, nil), 2*maxFramesPerMessage))
		})},
		{"masked server frame", upgraded(func(sc *serverConn) {
			_ = sc.send([]byte{0x81, 0x82, 1, 2, 3, 4, 'x', 'y'})
		})},
		{"reserved bits set", upgraded(func(sc *serverConn) {
			_ = sc.send([]byte{0xF1, 0x02, '{', '}'})
		})},
		{"fragmented control frame", upgraded(func(sc *serverConn) {
			_ = sc.send(frame(false, opPing, []byte("p")))
		})},
		{"continuation without start", upgraded(func(sc *serverConn) {
			_ = sc.send(frame(true, opContinuation, []byte("{}")))
		})},
		{"close before snapshot", upgraded(func(sc *serverConn) {
			_ = sc.send(frame(true, opClose, []byte{0x03, 0xE8}))
		})},
		{"connection reset", func(sc *serverConn) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []stateCommand
			var mu sync.Mutex
			srv := startFakeFAH(t, func(sc *serverConn) {
				tc.handler(sc)
				// Record any command that slipped through, then hang up.
				_ = sc.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				for {
					op, payload, err := sc.readFrame()
					if err != nil || op == opClose {
						return
					}
					var cmd stateCommand
					if json.Unmarshal(payload, &cmd) == nil && cmd.Cmd != "" {
						mu.Lock()
						got = append(got, cmd)
						mu.Unlock()
					}
				}
			})
			c, err := NewClientV8(srv.endpoint)
			if err != nil {
				t.Fatalf("NewClientV8: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			st, err := c.GetStatus(ctx)
			if err == nil {
				t.Fatalf("GetStatus succeeded against a hostile server: %+v", st)
			}
			if st.State != StateUnknown {
				t.Errorf("GetStatus state on error = %q, want %q", st.State, StateUnknown)
			}
			if err := c.Pause(ctx); err == nil {
				t.Fatal("Pause succeeded against a hostile server")
			}
			if ctx.Err() != nil {
				t.Fatalf("hostile server forced the call to its deadline: %v", ctx.Err())
			}

			srv.ln.Close()
			srv.wg.Wait()
			mu.Lock()
			defer mu.Unlock()
			if len(got) != 0 {
				t.Fatalf("client sent commands to a server that never proved to be FAH: %+v", got)
			}
		})
	}
}

// The client must answer pings that arrive before the snapshot and still
// read the state.
func TestClientV8AnswersPing(t *testing.T) {
	pong := make(chan []byte, 1)
	srv := startFakeFAH(t, func(sc *serverConn) {
		if sc.upgrade() != nil || sc.send(frame(true, opPing, []byte("hi"))) != nil {
			return
		}
		if op, payload, err := sc.readFrame(); err == nil && op == opPong {
			pong <- payload
		}
		_ = sc.sendText(`{"config":{"paused":false},"units":[]}`)
		_, _, _ = sc.readFrame()
	})
	c, err := NewClientV8(srv.endpoint)
	if err != nil {
		t.Fatalf("NewClientV8: %v", err)
	}
	st, err := c.GetStatus(context.Background())
	if err != nil || st != (Status{State: StateIdle}) {
		t.Fatalf("GetStatus = %+v, %v; want idle", st, err)
	}
	select {
	case p := <-pong:
		if string(p) != "hi" {
			t.Fatalf("pong payload = %q, want %q", p, "hi")
		}
	default:
		t.Fatal("client did not answer the ping")
	}
}

// --- Parser -----------------------------------------------------------------

func TestParseFAHMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want Status
	}{
		{"folding", `{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":18626}}]}`,
			Status{ActiveProjectID: 18626, State: StateFolding}},
		{"paused", `{"groups":{"":{"config":{"paused":true}}},"units":[{"state":"RUN","pause_reason":"Paused","assignment":{"project":18626}}]}`,
			Status{Paused: true, State: StatePaused}},
		{"one of two groups paused", `{"groups":{"a":{"config":{"paused":true}},"b":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":7}},{"state":"RUN","pause_reason":"x","assignment":{"project":9}}]}`,
			Status{ActiveProjectID: 7, State: StateFolding}},
		{"idle: downloading", `{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"DOWNLOAD","assignment":{"project":18626}}]}`,
			Status{State: StateIdle}},
		{"idle: no units", `{"groups":{"":{"config":{}}}}`, Status{State: StateIdle}},
		{"legacy top-level config", `{"config":{"paused":true},"units":[]}`, Status{Paused: true, State: StatePaused}},
		{"unknown fields ignored", ` {"info":{"version":"8.5.6"},"config":{"paused":false,"cpus":8},"units":[{"state":"RUN","assignment":{"project":1}}],"extra":[1,2]} `,
			Status{ActiveProjectID: 1, State: StateFolding}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFAHMessage([]byte(tc.msg))
			if err != nil || got != tc.want {
				t.Fatalf("parseFAHMessage = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}

	for _, msg := range []string{``, `   `, `["units",0,"state","RUN"]`, `"ping"`, `42`, `null`} {
		if _, err := parseFAHMessage([]byte(msg)); !errors.Is(err, errNotSnapshot) {
			t.Errorf("parseFAHMessage(%q) error = %v, want errNotSnapshot", msg, err)
		}
	}
	for _, msg := range []string{`{`, `{}`, `{"config":null}`, `{"groups":{}}`, `{"units":"none","config":{}}`} {
		if st, err := parseFAHMessage([]byte(msg)); err == nil || st.State != StateUnknown {
			t.Errorf("parseFAHMessage(%q) = %+v, %v; want unknown with error", msg, st, err)
		}
	}
}

func FuzzParseFAHMessage(f *testing.F) {
	f.Add([]byte(`{"groups":{"":{"config":{"paused":false}}},"units":[{"state":"RUN","assignment":{"project":18626}}]}`))
	f.Add([]byte(`{"config":{"paused":true},"units":[]}`))
	f.Add([]byte(`["groups","","config","paused",true]`))
	f.Add([]byte(`{"groups":{"a":{"config":{"paused":true}},"b":{}},"units":[{"state":"RUN","pause_reason":"x"}]}`))
	f.Add([]byte(`{"units":[{"assignment":{"project":-5},"state":"RUN"}],"config":{}}`))
	f.Add([]byte{0xFF, '{', 0x00})

	valid := map[string]bool{StateFolding: true, StatePaused: true, StateIdle: true}
	f.Fuzz(func(t *testing.T, msg []byte) {
		st, err := parseFAHMessage(msg)
		if err != nil {
			if st.State != StateUnknown {
				t.Fatalf("error %v with state %q, want %q", err, st.State, StateUnknown)
			}
			return
		}
		if !valid[st.State] {
			t.Fatalf("invalid state %q", st.State)
		}
		if st.Paused != (st.State == StatePaused) {
			t.Fatalf("Paused=%t inconsistent with state %q", st.Paused, st.State)
		}
		if (st.ActiveProjectID > 0) != (st.State == StateFolding) {
			t.Fatalf("ActiveProjectID=%d inconsistent with state %q", st.ActiveProjectID, st.State)
		}
	})
}

// --- MockController ---------------------------------------------------------

func TestMockController(t *testing.T) {
	m := NewMockController(18626)
	ctx := context.Background()

	if st, _ := m.GetStatus(ctx); st != (Status{ActiveProjectID: 18626, State: StateFolding}) {
		t.Fatalf("initial status = %+v", st)
	}
	if err := m.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if st, _ := m.GetStatus(ctx); st != (Status{Paused: true, State: StatePaused}) {
		t.Fatalf("after Pause = %+v", st)
	}
	if err := m.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if st, _ := m.GetStatus(ctx); st != (Status{ActiveProjectID: 18626, State: StateFolding}) {
		t.Fatalf("after Resume = %+v", st)
	}

	boom := errors.New("fah not running")
	m.SetError(boom)
	if err := m.Pause(ctx); !errors.Is(err, boom) {
		t.Fatalf("Pause with injected error = %v", err)
	}
	if st, err := m.GetStatus(ctx); !errors.Is(err, boom) || st.State != StateUnknown {
		t.Fatalf("GetStatus with injected error = %+v, %v", st, err)
	}
	m.SetError(nil)

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := m.Resume(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resume with canceled ctx = %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Pause(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Pause after Close = %v", err)
	}
	if idle := NewMockController(0); idle.Pause(ctx) != nil || idle.Resume(ctx) != nil {
		t.Fatal("idle mock failed")
	} else if st, _ := idle.GetStatus(ctx); st.State != StateIdle {
		t.Fatalf("idle mock after Resume = %+v", st)
	}
}

func TestMockControllerConcurrent(t *testing.T) {
	m := NewMockController(18626)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for j := range 200 {
				switch (i + j) % 4 {
				case 0:
					_ = m.Pause(ctx)
				case 1:
					_ = m.Resume(ctx)
				case 2:
					if st, err := m.GetStatus(ctx); err != nil || (st.State != StatePaused && st.State != StateFolding) {
						t.Errorf("GetStatus = %+v, %v", st, err)
					}
				default:
					m.SetError(nil)
				}
			}
		}()
	}
	wg.Wait()
}
