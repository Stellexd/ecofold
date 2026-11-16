package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shortDir returns a private temporary directory with a short path:
// t.TempDir() embeds the test name and overflows sun_path (104 bytes on
// macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ef")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// echoHandler answers every command with its name and args. It can panic or
// block on demand.
type echoHandler struct {
	calls   atomic.Int64
	panicOn string
	block   chan struct{} // when non-nil, force_pause waits for it or for ctx
}

type echo struct {
	Cmd  string          `json:"cmd"`
	Args json.RawMessage `json:"args,omitempty"`
}

func (h *echoHandler) Handle(ctx context.Context, req Request) Response {
	h.calls.Add(1)
	if req.Cmd == h.panicOn {
		panic("handler exploded")
	}
	if h.block != nil && req.Cmd == CmdForcePause {
		select {
		case <-h.block:
		case <-ctx.Done():
		}
	}
	if req.Cmd == CmdSetEcoWatts {
		var a struct {
			Watts int `json:"watts"`
		}
		if err := DecodeArgs(req.Args, &a); err != nil {
			return Fail(CodeBadRequest, "bad args")
		}
		if a.Watts > 145 {
			return Fail(CodeOutOfRange, "watts above 145")
		}
	}
	return OK(echo{Cmd: req.Cmd, Args: req.Args})
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testServer struct {
	*Server
	logs *logBuffer
	done chan error
}

// startServer listens on a fresh socket and serves until the test ends.
// tune runs between Listen and Serve.
func startServer(t *testing.T, h Handler, tune func(*Server)) *testServer {
	t.Helper()
	return startServerAt(t, filepath.Join(shortDir(t), "d", "ecofold.sock"), h, tune)
}

func startServerAt(t *testing.T, path string, h Handler, tune func(*Server)) *testServer {
	t.Helper()
	logs := &logBuffer{}
	s, err := Listen(Config{
		Path:    path,
		Handler: h,
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if tune != nil {
		tune(s)
	}
	ts := &testServer{Server: s, logs: logs, done: make(chan error, 1)}
	go func() { ts.done <- s.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return ts
}

// rawConn is a client that speaks bytes, for requests ipc.Client refuses to
// build.
type rawConn struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func dialRaw(t *testing.T, path string) *rawConn {
	t.Helper()
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &rawConn{t: t, c: c, r: bufio.NewReader(c)}
}

func (rc *rawConn) send(b []byte) {
	rc.t.Helper()
	_ = rc.c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := rc.c.Write(b); err != nil {
		rc.t.Fatalf("write: %v", err)
	}
}

func (rc *rawConn) recv() Response {
	rc.t.Helper()
	resp, err := rc.tryRecv(5 * time.Second)
	if err != nil {
		rc.t.Fatalf("read: %v", err)
	}
	return resp
}

func (rc *rawConn) tryRecv(timeout time.Duration) (Response, error) {
	_ = rc.c.SetReadDeadline(time.Now().Add(timeout))
	line, err := rc.r.ReadBytes('\n')
	if err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("response is not JSON: %q", line)
	}
	return resp, nil
}

// expectClosed asserts the server closes the connection without sending
// anything more.
func (rc *rawConn) expectClosed(timeout time.Duration) {
	rc.t.Helper()
	_ = rc.c.SetReadDeadline(time.Now().Add(timeout))
	b, err := io.ReadAll(rc.r)
	if err != nil && !isReset(err) {
		rc.t.Fatalf("la conexión no se cerró: %v", err)
	}
	if len(b) != 0 {
		rc.t.Fatalf("datos inesperados antes del cierre: %q", b)
	}
}

// isReset reports a connection reset: closing a socket with unread input
// resets it instead of ending it cleanly.
func isReset(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection reset")
}

func wantCode(t *testing.T, resp Response, code string) {
	t.Helper()
	if resp.OK || resp.Error == nil || resp.Error.Code != code {
		t.Fatalf("respuesta = %+v (error %+v), se esperaba el código %q", resp, resp.Error, code)
	}
	if resp.V != Version {
		t.Fatalf("v = %d, se esperaba %d", resp.V, Version)
	}
}

// TestRoundTripAllCommands runs the five commands through Client on one
// connection.
func TestRoundTripAllCommands(t *testing.T) {
	h := &echoHandler{}
	ts := startServer(t, h, nil)

	c, err := Dial(context.Background(), ts.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	cases := []struct {
		cmd  string
		args any
	}{
		{CmdStatus, nil},
		{CmdForcePause, nil},
		{CmdForceResume, nil},
		{CmdSetPauseProcesses, map[string]any{"patterns": []string{"blender", "obs*"}}},
		{CmdSetEcoWatts, map[string]any{"watts": 123}},
	}
	for _, tc := range cases {
		raw, err := c.Call(context.Background(), tc.cmd, tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		var got echo
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: data %q: %v", tc.cmd, raw, err)
		}
		if got.Cmd != tc.cmd {
			t.Fatalf("eco = %q, se esperaba %q", got.Cmd, tc.cmd)
		}
		if tc.args != nil {
			want, _ := json.Marshal(tc.args)
			if !bytes.Equal(got.Args, want) {
				t.Fatalf("%s: args = %s, se esperaba %s", tc.cmd, got.Args, want)
			}
		}
	}
	if got := h.calls.Load(); got != int64(len(cases)) {
		t.Fatalf("el handler recibió %d requests, se esperaban %d", got, len(cases))
	}

	_, err = c.Call(context.Background(), CmdSetEcoWatts, map[string]any{"watts": 500})
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodeOutOfRange {
		t.Fatalf("err = %v, se esperaba RemoteError out_of_range", err)
	}
}

// TestIDsAreEchoed sends several requests on one connection, some pipelined,
// and checks each response carries its request's id, in order.
func TestIDsAreEchoed(t *testing.T) {
	ts := startServer(t, &echoHandler{}, nil)
	rc := dialRaw(t, ts.Path())

	rc.send([]byte(`{"v":1,"id":"a-1","cmd":"status"}` + "\n"))
	if resp := rc.recv(); !resp.OK || resp.ID != "a-1" {
		t.Fatalf("respuesta = %+v", resp)
	}

	rc.send([]byte(`{"v":1,"id":"b","cmd":"force_pause"}` + "\n" +
		`{"v":1,"id":"","cmd":"status","args":{}}` + "\n" +
		`{"v":1,"id":"` + strings.Repeat("x", MaxIDLen) + `","cmd":"force_resume","args":null}` + "\n"))
	for _, want := range []string{"b", "", strings.Repeat("x", MaxIDLen)} {
		if resp := rc.recv(); !resp.OK || resp.ID != want {
			t.Fatalf("respuesta = %+v, se esperaba el id %q", resp, want)
		}
	}
}

// TestMalformedRequests checks that every broken request gets an error
// response, never a panic, and that the connection stays usable.
func TestMalformedRequests(t *testing.T) {
	h := &echoHandler{}
	ts := startServer(t, h, nil)
	rc := dialRaw(t, ts.Path())

	cases := []struct {
		name string
		line string
		code string
		id   string
	}{
		{"json corrupto", `{"v":1,"id":"x","cmd":`, CodeBadRequest, ""},
		{"null", `null`, CodeBadRequest, ""},
		{"array", `[1,2,3]`, CodeBadRequest, ""},
		{"string", `"status"`, CodeBadRequest, ""},
		{"línea vacía", ``, CodeBadRequest, ""},
		{"campo desconocido", `{"v":1,"id":"x","cmd":"status","extra":true}`, CodeBadRequest, ""},
		{"versión 2", `{"v":2,"id":"v2","cmd":"status"}`, CodeBadRequest, "v2"},
		{"sin versión", `{"id":"nov","cmd":"status"}`, CodeBadRequest, "nov"},
		{"tipo incorrecto", `{"v":"1","id":"x","cmd":"status"}`, CodeBadRequest, ""},
		{"id largo", `{"v":1,"id":"` + strings.Repeat("x", MaxIDLen+1) + `","cmd":"status"}`, CodeBadRequest, ""},
		{"comando desconocido", `{"v":1,"id":"u","cmd":"reboot"}`, CodeUnknownCmd, "u"},
		{"comando vacío", `{"v":1,"id":"e"}`, CodeUnknownCmd, "e"},
		{"datos sobrantes", `{"v":1,"id":"x","cmd":"status"} {"v":1}`, CodeBadRequest, ""},
		{"binario", "\x00\xff\xfe\x01\x02\x80garbage\x7f", CodeBadRequest, ""},
		{"utf-8 inválido", "{\"v\":1,\"id\":\"\xff\",\"cmd\":\"nope\"}", CodeUnknownCmd, "�"},
		{"anidado profundo", strings.Repeat("[", 5000), CodeBadRequest, ""},
	}
	for _, tc := range cases {
		rc.send([]byte(tc.line + "\n"))
		resp := rc.recv()
		if resp.OK || resp.Error == nil || resp.Error.Code != tc.code {
			t.Fatalf("%s: respuesta = %+v (error %+v), se esperaba %q", tc.name, resp, resp.Error, tc.code)
		}
		if resp.ID != tc.id {
			t.Fatalf("%s: id = %q, se esperaba %q", tc.name, resp.ID, tc.id)
		}
	}
	if got := h.calls.Load(); got != 0 {
		t.Fatalf("el handler recibió %d requests inválidas", got)
	}

	rc.send([]byte(`{"v":1,"id":"ok","cmd":"status"}` + "\n"))
	if resp := rc.recv(); !resp.OK || resp.ID != "ok" {
		t.Fatalf("la conexión no sobrevivió a las requests inválidas: %+v", resp)
	}
}

// TestTooLargeMessage checks the 64 KiB cap: one byte over gets too_large and
// the connection is closed; exactly at the limit is still parsed.
func TestTooLargeMessage(t *testing.T) {
	h := &echoHandler{}
	ts := startServer(t, h, nil)

	t.Run("límite exacto", func(t *testing.T) {
		rc := dialRaw(t, ts.Path())
		// Padded with insignificant whitespace up to the limit.
		prefix, suffix := `{"v":1,"id":"big","cmd":"status"`, `}`
		line := prefix + strings.Repeat(" ", MaxMessage-len(prefix)-len(suffix)) + suffix
		if len(line) != MaxMessage {
			t.Fatalf("línea de %d bytes", len(line))
		}
		rc.send([]byte(line + "\n"))
		if resp := rc.recv(); !resp.OK || resp.ID != "big" {
			t.Fatalf("respuesta = %+v (error %+v)", resp, resp.Error)
		}
	})

	t.Run("un byte más con salto de línea", func(t *testing.T) {
		rc := dialRaw(t, ts.Path())
		rc.send(append(bytes.Repeat([]byte("a"), MaxMessage+1), '\n'))
		wantCode(t, rc.recv(), CodeTooLarge)
		rc.expectClosed(3 * time.Second)
	})

	t.Run("sin salto de línea", func(t *testing.T) {
		rc := dialRaw(t, ts.Path())
		rc.send(bytes.Repeat([]byte("a"), MaxMessage+1))
		wantCode(t, rc.recv(), CodeTooLarge)
		rc.expectClosed(3 * time.Second)
	})

	t.Run("flujo sin fin", func(t *testing.T) {
		rc := dialRaw(t, ts.Path())
		// The server stops reading at the limit, so the writer may block or
		// fail: both are fine as long as the server answers and hangs up.
		go func() {
			_ = rc.c.SetWriteDeadline(time.Now().Add(3 * time.Second))
			_, _ = rc.c.Write(bytes.Repeat([]byte("{"), 4<<20))
		}()
		resp, err := rc.tryRecv(5 * time.Second)
		if err != nil {
			if isReset(err) {
				return // the reset raced the response; the server did hang up
			}
			t.Fatalf("read: %v", err)
		}
		wantCode(t, resp, CodeTooLarge)
	})

	if got := h.calls.Load(); got != 1 {
		t.Fatalf("el handler recibió %d requests, se esperaba 1", got)
	}
}

// TestSlowClientIsDisconnected checks the read deadline: a client that never
// finishes its line, or that idles, is dropped after about 2 s.
func TestSlowClientIsDisconnected(t *testing.T) {
	ts := startServer(t, &echoHandler{}, nil)

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"sin salto de línea", `{"v":1,"id":"slow","cmd":"status"}`},
		{"ociosa", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc := dialRaw(t, ts.Path())
			start := time.Now()
			if tc.payload != "" {
				rc.send([]byte(tc.payload))
			}
			rc.expectClosed(5 * time.Second)
			if d := time.Since(start); d < 1500*time.Millisecond || d > 4*time.Second {
				t.Fatalf("cerrada tras %v, se esperaban ~2 s", d)
			}
		})
	}
}

// TestTricklingClientIsDisconnected checks the deadline covers the whole
// message: sending a byte now and then does not keep the connection open.
func TestTricklingClientIsDisconnected(t *testing.T) {
	ts := startServer(t, &echoHandler{}, func(s *Server) { s.ioTimeout = 400 * time.Millisecond })
	rc := dialRaw(t, ts.Path())

	start := time.Now()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := rc.c.Write([]byte("a")); err != nil {
					return
				}
			}
		}
	}()
	_ = rc.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := rc.r.ReadByte(); err == nil {
		t.Fatal("se esperaba el cierre de la conexión")
	} else if ne := net.Error(nil); errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("la conexión siguió abierta mientras goteaban bytes")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cerrada tras %v con un límite de 400 ms", d)
	}
}

// TestUnauthorizedPeerGetsNoResponse checks a refused peer is disconnected
// before any byte is read or written, and that the refusal is logged once.
func TestUnauthorizedPeerGetsNoResponse(t *testing.T) {
	h := &echoHandler{}
	var seen atomic.Int64
	ts := startServer(t, h, func(s *Server) {
		s.authorize = func(PeerCred) bool { seen.Add(1); return false }
	})

	for i := 0; i < 5; i++ {
		rc := dialRaw(t, ts.Path())
		// The write may fail if the server already hung up.
		_, _ = rc.c.Write([]byte(`{"v":1,"id":"x","cmd":"force_pause"}` + "\n"))
		rc.expectClosed(3 * time.Second)
	}
	if got := h.calls.Load(); got != 0 {
		t.Fatalf("el handler recibió %d requests de un peer no autorizado", got)
	}
	if got := seen.Load(); got != 5 {
		t.Fatalf("authorize se llamó %d veces, se esperaban 5", got)
	}
	if n := strings.Count(ts.logs.String(), "peer not authorized"); n != 1 {
		t.Fatalf("%d avisos de rechazo, se esperaba 1 (rate limit):\n%s", n, ts.logs.String())
	}
	if !strings.Contains(ts.logs.String(), fmt.Sprintf("uid=%d", os.Getuid())) {
		t.Fatalf("el aviso no incluye el uid:\n%s", ts.logs.String())
	}
}

// TestPeerCredentialsAreReal checks the kernel-reported credentials against
// this process: the client and the server are the same process here.
func TestPeerCredentialsAreReal(t *testing.T) {
	got := make(chan PeerCred, 1)
	ts := startServer(t, &echoHandler{}, func(s *Server) {
		s.authorize = func(c PeerCred) bool { got <- c; return true }
	})
	rc := dialRaw(t, ts.Path())
	rc.send([]byte(`{"v":1,"id":"x","cmd":"status"}` + "\n"))
	rc.recv()

	cred := <-got
	if cred.UID != uint32(os.Getuid()) {
		t.Fatalf("uid = %d, se esperaba %d", cred.UID, os.Getuid())
	}
	if cred.GID != uint32(os.Getgid()) {
		t.Fatalf("gid = %d, se esperaba %d", cred.GID, os.Getgid())
	}
	if cred.PID != int32(os.Getpid()) {
		t.Fatalf("pid = %d, se esperaba %d", cred.PID, os.Getpid())
	}
}

// TestDefaultAuthorizerAcceptsOwnUser checks the platform authorizer lets the
// daemon's own user in.
func TestDefaultAuthorizerAcceptsOwnUser(t *testing.T) {
	ts := startServer(t, &echoHandler{}, nil)
	rc := dialRaw(t, ts.Path())
	rc.send([]byte(`{"v":1,"id":"me","cmd":"status"}` + "\n"))
	if resp := rc.recv(); !resp.OK {
		t.Fatalf("respuesta = %+v", resp)
	}
}

// TestConcurrentClients runs 20 clients with 50 mixed requests each. Run it
// with -race.
func TestConcurrentClients(t *testing.T) {
	h := &echoHandler{}
	ts := startServer(t, h, nil)

	const clients, requests = 20, 50
	cmds := []string{CmdStatus, CmdForcePause, CmdForceResume, CmdSetPauseProcesses, CmdSetEcoWatts}
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := Dial(context.Background(), ts.Path())
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			for j := 0; j < requests; j++ {
				cmd := cmds[(i+j)%len(cmds)]
				var args any
				if cmd == CmdSetEcoWatts {
					args = map[string]int{"watts": 100 + j}
				}
				raw, err := c.Call(context.Background(), cmd, args)
				var re *RemoteError
				if errors.As(err, &re) && cmd == CmdSetEcoWatts && 100+j > 145 {
					continue
				}
				if err != nil {
					errs <- fmt.Errorf("cliente %d request %d (%s): %w", i, j, cmd, err)
					return
				}
				var got echo
				if err := json.Unmarshal(raw, &got); err != nil || got.Cmd != cmd {
					errs <- fmt.Errorf("cliente %d request %d: eco %q, se esperaba %q", i, j, got.Cmd, cmd)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := h.calls.Load(); got != clients*requests {
		t.Fatalf("el handler recibió %d requests, se esperaban %d", got, clients*requests)
	}
}

// TestConnectionLimit checks the 33rd concurrent connection is closed at once
// and that a slot frees up when a client leaves.
func TestConnectionLimit(t *testing.T) {
	ts := startServer(t, &echoHandler{}, func(s *Server) { s.ioTimeout = 10 * time.Second })

	conns := make([]*rawConn, defaultMaxConns)
	for i := range conns {
		conns[i] = dialRaw(t, ts.Path())
		// A response proves the connection holds a slot.
		conns[i].send([]byte(`{"v":1,"id":"x","cmd":"status"}` + "\n"))
		conns[i].recv()
	}

	extra := dialRaw(t, ts.Path())
	_, _ = extra.c.Write([]byte(`{"v":1,"id":"x","cmd":"status"}` + "\n"))
	extra.expectClosed(3 * time.Second)
	if !strings.Contains(ts.logs.String(), "connection limit reached") {
		t.Fatalf("no se registró el rechazo:\n%s", ts.logs.String())
	}

	_ = conns[0].c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rc := dialRaw(t, ts.Path())
		_, _ = rc.c.Write([]byte(`{"v":1,"id":"again","cmd":"status"}` + "\n"))
		if resp, err := rc.tryRecv(time.Second); err == nil && resp.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no se liberó un hueco tras cerrar una conexión")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHandlerPanicClosesOnlyThatConnection checks the per-connection recover.
func TestHandlerPanicClosesOnlyThatConnection(t *testing.T) {
	ts := startServer(t, &echoHandler{panicOn: CmdForceResume}, nil)

	other := dialRaw(t, ts.Path())
	other.send([]byte(`{"v":1,"id":"1","cmd":"status"}` + "\n"))
	other.recv()

	rc := dialRaw(t, ts.Path())
	rc.send([]byte(`{"v":1,"id":"boom","cmd":"force_resume"}` + "\n"))
	rc.expectClosed(3 * time.Second)
	if !strings.Contains(ts.logs.String(), "panic while serving a connection") {
		t.Fatalf("el panic no se registró:\n%s", ts.logs.String())
	}

	other.send([]byte(`{"v":1,"id":"2","cmd":"status"}` + "\n"))
	if resp := other.recv(); !resp.OK || resp.ID != "2" {
		t.Fatalf("otra conexión afectada por el panic: %+v", resp)
	}
	fresh := dialRaw(t, ts.Path())
	fresh.send([]byte(`{"v":1,"id":"3","cmd":"status"}` + "\n"))
	if resp := fresh.recv(); !resp.OK {
		t.Fatalf("el servidor no aceptó conexiones tras el panic: %+v", resp)
	}
}

// TestShutdown checks Shutdown closes open connections, unblocks a handler
// through its context, stops Serve, removes the socket and is idempotent.
func TestShutdown(t *testing.T) {
	h := &echoHandler{block: make(chan struct{})}
	ts := startServer(t, h, nil)

	idle := dialRaw(t, ts.Path())
	idle.send([]byte(`{"v":1,"id":"1","cmd":"status"}` + "\n"))
	idle.recv()
	busy := dialRaw(t, ts.Path())
	busy.send([]byte(`{"v":1,"id":"2","cmd":"force_pause"}` + "\n"))
	for h.calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := ts.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Fatalf("Shutdown tardó %v", d)
	}
	select {
	case err := <-ts.done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve no terminó tras Shutdown")
	}
	if _, err := os.Lstat(ts.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("el socket sigue en disco tras Shutdown: %v", err)
	}
	idle.expectClosed(time.Second)
	if _, err := net.DialTimeout("unix", ts.Path(), 200*time.Millisecond); err == nil {
		t.Fatal("se pudo conectar tras Shutdown")
	}
	if err := ts.Shutdown(context.Background()); err != nil {
		t.Fatalf("segundo Shutdown: %v", err)
	}
}

// TestShutdownBoundedByContext checks Shutdown gives up on a handler that
// ignores its context.
func TestShutdownBoundedByContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	ts := startServer(t, handlerFunc(func(context.Context, Request) Response {
		<-release
		return OK(nil)
	}), nil)

	rc := dialRaw(t, ts.Path())
	rc.send([]byte(`{"v":1,"id":"1","cmd":"status"}` + "\n"))
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := ts.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, se esperaba DeadlineExceeded", err)
	}
	if _, err := os.Lstat(ts.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("el socket sigue en disco: %v", err)
	}
}

type handlerFunc func(context.Context, Request) Response

func (f handlerFunc) Handle(ctx context.Context, req Request) Response { return f(ctx, req) }

// TestOversizedResponseBecomesInternalError checks a handler cannot make the
// server emit a line the client would refuse.
func TestOversizedResponseBecomesInternalError(t *testing.T) {
	ts := startServer(t, handlerFunc(func(context.Context, Request) Response {
		return OK(strings.Repeat("x", MaxMessage))
	}), nil)
	c, err := Dial(context.Background(), ts.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	_, err = c.Call(context.Background(), CmdStatus, nil)
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodeInternal {
		t.Fatalf("err = %v, se esperaba RemoteError internal", err)
	}
}
