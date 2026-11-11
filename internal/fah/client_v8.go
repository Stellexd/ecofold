package fah

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultEndpoint is the WebSocket API of a local FAH v8 client.
const DefaultEndpoint = "ws://127.0.0.1:7396/api/websocket"

const (
	defaultPort    = "7396"
	defaultPath    = "/api/websocket"
	defaultTimeout = 3 * time.Second

	// maxMessageBytes caps one WebSocket message (all its fragments). The FAH
	// state snapshot is a few KiB; the cap keeps a hostile server from making
	// the daemon allocate without bound.
	maxMessageBytes = 1 << 20 // 1 MiB

	// maxFramesPerMessage bounds a stream of empty fragments or control frames
	// so a hostile server cannot spin the reader until the deadline.
	maxFramesPerMessage = 1024

	// maxSkippedMessages is how many non-snapshot messages are tolerated
	// before the state snapshot, which FAH v8 sends first.
	maxSkippedMessages = 4

	// Handshake response limits: one line must fit in the bufio buffer.
	handshakeBufSize    = 4096
	maxHandshakeBytes   = 16 << 10
	maxHandshakeHeaders = 64

	// closeDrainTimeout bounds the wait for the server's close frame.
	closeDrainTimeout = 250 * time.Millisecond

	// confirmInterval is the pause between state reads while confirming that
	// FAH applied a command.
	confirmInterval = 50 * time.Millisecond

	// websocketGUID is the fixed RFC 6455 handshake GUID.
	websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// WebSocket opcodes (RFC 6455 section 5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

var (
	errNotSnapshot  = errors.New("fah: message is not a state snapshot")
	errPeerClosed   = errors.New("fah: connection closed by server")
	errMessageLarge = fmt.Errorf("fah: message exceeds %d bytes", maxMessageBytes)
)

// ClientV8 controls a local FAH v8 client through its WebSocket API. Each
// call opens a short-lived connection, reads the state snapshot the client
// sends first, optionally sends one command and closes. No goroutine outlives
// a call. ClientV8 is safe for concurrent use.
type ClientV8 struct {
	dialAddr string // address actually dialed; localhost is pinned to 127.0.0.1
	host     string // Host header
	path     string // request URI
	timeout  time.Duration

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

var _ Controller = (*ClientV8)(nil)

// NewClientV8 returns a client for endpoint, or DefaultEndpoint when endpoint
// is empty. Only ws:// URLs whose host is exactly 127.0.0.1, ::1 or localhost
// are accepted, so the controller can never be pointed at another machine.
func NewClientV8(endpoint string) (*ClientV8, error) {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	dialAddr, host, path, err := validateEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	return &ClientV8{
		dialAddr: dialAddr,
		host:     host,
		path:     path,
		timeout:  defaultTimeout,
		conns:    make(map[net.Conn]struct{}),
	}, nil
}

// validateEndpoint enforces the loopback-only policy and returns the address
// to dial, the Host header and the request URI.
func validateEndpoint(endpoint string) (dialAddr, host, path string, err error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", "", fmt.Errorf("fah: invalid endpoint: %w", err)
	}
	if u.Scheme != "ws" {
		return "", "", "", fmt.Errorf("fah: endpoint scheme %q not allowed: use ws://", u.Scheme)
	}
	if u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return "", "", "", errors.New("fah: endpoint must not contain user info, opaque data or a fragment")
	}

	hostname := u.Hostname()
	var dialHost string
	switch {
	case hostname == "127.0.0.1":
		dialHost = hostname
	case hostname == "::1":
		dialHost = hostname
	case strings.EqualFold(hostname, "localhost"):
		// Never resolve the name: a tampered resolver or /etc/hosts could map
		// it to a non-loopback address.
		dialHost = "127.0.0.1"
	default:
		return "", "", "", fmt.Errorf("fah: endpoint host %q is not loopback (allowed: 127.0.0.1, ::1, localhost)", hostname)
	}

	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "", "", fmt.Errorf("fah: invalid endpoint port %q", port)
	}

	path = u.RequestURI()
	if u.Path == "" {
		path = defaultPath
	}
	// net/url keeps the raw query, which may contain spaces; any byte at or
	// below 0x20 (or DEL) would corrupt the HTTP request line.
	if strings.ContainsFunc(path, func(r rune) bool { return r <= ' ' || r == 0x7F }) {
		return "", "", "", errors.New("fah: endpoint path or query contains spaces or control characters")
	}
	return net.JoinHostPort(dialHost, port), net.JoinHostPort(hostname, port), path, nil
}

// Pause pauses folding in every resource group. It returns nil only after a
// fresh state snapshot confirms that FAH is paused.
func (c *ClientV8) Pause(ctx context.Context) error {
	return c.sendState(ctx, "pause")
}

// Resume resumes folding in every resource group. It returns nil only after a
// fresh state snapshot confirms that FAH is no longer paused.
func (c *ClientV8) Resume(ctx context.Context) error {
	return c.sendState(ctx, "fold")
}

// GetStatus reads the client's state snapshot. On error the returned status
// has State set to StateUnknown.
func (c *ClientV8) GetStatus(ctx context.Context) (Status, error) {
	ctx, cancel := c.withDefaultTimeout(ctx)
	defer cancel()

	var st Status
	err := c.do(ctx, "status", func(ws *wsConn) error {
		var err error
		st, err = ws.readStatus()
		return err
	})
	if err != nil {
		return Status{State: StateUnknown}, err
	}
	return st, nil
}

// Close aborts in-flight calls and makes every later call return ErrClosed.
// It is idempotent.
func (c *ClientV8) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	for conn := range c.conns {
		_ = conn.Close()
	}
	return nil
}

// stateCommand is the FAH v8 state command. The Web Control sends the same
// fields, with time as an ISO 8601 UTC timestamp; omitting "group" applies
// the command to every resource group.
type stateCommand struct {
	Cmd   string `json:"cmd"`
	State string `json:"state"`
	Time  string `json:"time"`
}

func (c *ClientV8) sendState(ctx context.Context, state string) error {
	cmd, err := json.Marshal(stateCommand{
		Cmd:   "state",
		State: state,
		Time:  time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	})
	if err != nil {
		return fmt.Errorf("fah: encode command: %w", err)
	}
	ctx, cancel := c.withDefaultTimeout(ctx)
	defer cancel()

	err = c.do(ctx, state, func(ws *wsConn) error {
		// Read the snapshot first: it proves the peer is a FAH client before
		// any command is sent, and leaves no unread data that would turn our
		// close into a TCP reset and drop the command on the server.
		if _, err := ws.readStatus(); err != nil {
			return err
		}
		return ws.writeFrame(opText, cmd)
	})
	if err != nil {
		return err
	}
	return c.confirmPaused(ctx, state, state == "pause")
}

// confirmPaused polls the state until Paused equals want or ctx expires. A
// successful write only means the bytes reached the local socket buffer; if
// FAH died or dropped the connection, the command was lost, and the daemon
// must not believe folding is paused when it is not.
func (c *ClientV8) confirmPaused(ctx context.Context, op string, want bool) error {
	var last Status
	var lastErr error
	for {
		last, lastErr = c.GetStatus(ctx)
		if lastErr == nil && last.Paused == want {
			return nil
		}
		if errors.Is(lastErr, ErrClosed) {
			return fmt.Errorf("fah: %s not confirmed: %w", op, ErrClosed)
		}
		t := time.NewTimer(confirmInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			if lastErr != nil {
				return fmt.Errorf("fah: %s not confirmed: %w (%v)", op, ctx.Err(), lastErr)
			}
			return fmt.Errorf("fah: %s not confirmed: %w (state %q)", op, ctx.Err(), last.State)
		case <-t.C:
		}
	}
}

// withDefaultTimeout bounds ctx by the default timeout when the caller set
// no deadline, so a whole call (including confirmation) stays bounded.
func (c *ClientV8) withDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}

// do runs fn over a fresh WebSocket connection bounded by ctx (or by the
// default timeout when ctx has no deadline) and closes it afterwards.
func (c *ClientV8) do(ctx context.Context, op string, fn func(*wsConn) error) error {
	if c.isClosed() {
		return ErrClosed
	}
	ctx, cancel := c.withDefaultTimeout(ctx)
	defer cancel()
	deadline, _ := ctx.Deadline()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.dialAddr)
	if err != nil {
		return c.wrapErr(ctx, deadline, op, fmt.Errorf("dial %s: %w", c.dialAddr, err))
	}
	if err := c.track(conn); err != nil {
		_ = conn.Close()
		return err
	}
	defer c.untrack(conn)

	_ = conn.SetDeadline(deadline)
	// Cancellation (not only the deadline) must unblock reads and writes.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	ws := &wsConn{conn: conn, br: bufio.NewReaderSize(conn, handshakeBufSize)}
	err = ws.handshake(c.host, c.path)
	if err == nil {
		err = fn(ws)
	}
	if err == nil {
		ws.closeGracefully(deadline)
		return nil
	}
	return c.wrapErr(ctx, deadline, op, err)
}

// wrapErr attributes a failed call to Close or to ctx when they caused it.
// The connection deadline equals the ctx deadline, so the I/O timeout can
// fire a moment before ctx.Err() becomes non-nil; such a timeout is still
// reported as context.DeadlineExceeded.
func (c *ClientV8) wrapErr(ctx context.Context, deadline time.Time, op string, err error) error {
	var ne net.Error
	switch {
	case c.isClosed():
		return fmt.Errorf("fah: %s: %w", op, ErrClosed)
	case ctx.Err() != nil:
		return fmt.Errorf("fah: %s: %w (%v)", op, ctx.Err(), err)
	case errors.As(err, &ne) && ne.Timeout() && !time.Now().Before(deadline):
		return fmt.Errorf("fah: %s: %w (%v)", op, context.DeadlineExceeded, err)
	default:
		return fmt.Errorf("fah: %s: %w", op, err)
	}
}

func (c *ClientV8) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *ClientV8) track(conn net.Conn) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	c.conns[conn] = struct{}{}
	return nil
}

func (c *ClientV8) untrack(conn net.Conn) {
	c.mu.Lock()
	delete(c.conns, conn)
	c.mu.Unlock()
	_ = conn.Close()
}

// wsConn is a minimal RFC 6455 client: text messages, fragmentation, ping/pong
// and close. Extensions and subprotocols are not negotiated.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func (w *wsConn) handshake(host, path string) error {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	key := base64.StdEncoding.EncodeToString(nonce[:])

	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(w.conn, req); err != nil {
		return fmt.Errorf("handshake write: %w", err)
	}

	sum := sha1.Sum([]byte(key + websocketGUID))
	return w.readHandshakeResponse(base64.StdEncoding.EncodeToString(sum[:]))
}

// readHandshakeResponse parses the HTTP upgrade response with hard limits on
// line length, header count and total size. Redirects and any status other
// than 101 are rejected.
func (w *wsConn) readHandshakeResponse(wantAccept string) error {
	budget := maxHandshakeBytes

	status, err := w.readHeaderLine(&budget)
	if err != nil {
		return err
	}
	proto, rest, _ := strings.Cut(status, " ")
	code, _, _ := strings.Cut(rest, " ")
	if !strings.HasPrefix(proto, "HTTP/1.") || code != "101" {
		return fmt.Errorf("handshake: unexpected status line %.64q", status)
	}

	var upgrade, connUpgrade, accept bool
	for i := 0; ; i++ {
		if i > maxHandshakeHeaders {
			return errors.New("handshake: too many headers")
		}
		line, err := w.readHeaderLine(&budget)
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("handshake: malformed header %.64q", line)
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "upgrade":
			upgrade = strings.EqualFold(value, "websocket")
		case "connection":
			for _, tok := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
					connUpgrade = true
				}
			}
		case "sec-websocket-accept":
			accept = value == wantAccept
		case "sec-websocket-extensions", "sec-websocket-protocol":
			return fmt.Errorf("handshake: server selected %s that was not requested", name)
		}
	}
	if !upgrade || !connUpgrade || !accept {
		return errors.New("handshake: missing or invalid upgrade headers")
	}
	return nil
}

func (w *wsConn) readHeaderLine(budget *int) (string, error) {
	line, err := w.br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errors.New("handshake: header line too long")
	}
	if err != nil {
		return "", fmt.Errorf("handshake read: %w", err)
	}
	*budget -= len(line)
	if *budget < 0 {
		return "", errors.New("handshake: response headers too large")
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// readMessage returns the next complete text message. Ping frames are
// answered, pong frames ignored, and a close frame ends the connection.
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	inMessage := false

	for frames := 0; frames < maxFramesPerMessage; frames++ {
		var hdr [2]byte
		if _, err := io.ReadFull(w.br, hdr[:]); err != nil {
			return nil, err
		}
		fin := hdr[0]&0x80 != 0
		op := hdr[0] & 0x0F
		if hdr[0]&0x70 != 0 {
			return nil, errors.New("frame uses reserved bits")
		}
		if hdr[1]&0x80 != 0 {
			return nil, errors.New("server frame is masked")
		}

		n := uint64(hdr[1] & 0x7F)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(ext[:])
		}

		if op >= opClose {
			if !fin || n > 125 {
				return nil, errors.New("invalid control frame")
			}
			payload := make([]byte, n)
			if _, err := io.ReadFull(w.br, payload); err != nil {
				return nil, err
			}
			switch op {
			case opClose:
				return nil, errPeerClosed
			case opPing:
				if err := w.writeFrame(opPong, payload); err != nil {
					return nil, err
				}
			case opPong:
			default:
				return nil, fmt.Errorf("unknown control opcode %#x", op)
			}
			continue
		}

		switch op {
		case opText:
			if inMessage {
				return nil, errors.New("new message before previous one finished")
			}
			inMessage = true
		case opContinuation:
			if !inMessage {
				return nil, errors.New("continuation frame without a message")
			}
		case opBinary:
			return nil, errors.New("unexpected binary message")
		default:
			return nil, fmt.Errorf("unknown data opcode %#x", op)
		}

		// Check the size before allocating anything.
		if n > maxMessageBytes || uint64(len(msg))+n > maxMessageBytes {
			return nil, errMessageLarge
		}
		start := len(msg)
		msg = append(msg, make([]byte, n)...)
		if _, err := io.ReadFull(w.br, msg[start:]); err != nil {
			return nil, err
		}
		if fin {
			return msg, nil
		}
	}
	return nil, errors.New("too many frames in one message")
}

// writeFrame sends one masked, final frame, as RFC 6455 requires of clients.
func (w *wsConn) writeFrame(op byte, payload []byte) error {
	buf := make([]byte, 0, 14+len(payload))
	buf = append(buf, 0x80|op)
	switch n := len(payload); {
	case n <= 125:
		buf = append(buf, 0x80|byte(n))
	case n <= 0xFFFF:
		buf = append(buf, 0x80|126)
		buf = binary.BigEndian.AppendUint16(buf, uint16(n))
	default:
		buf = append(buf, 0x80|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}

	var mask [4]byte
	_, _ = rand.Read(mask[:])
	buf = append(buf, mask[:]...)
	for i, b := range payload {
		buf = append(buf, b^mask[i%4])
	}

	_, err := w.conn.Write(buf)
	return err
}

// readStatus reads messages until the state snapshot, which FAH v8 sends
// first, and parses it.
func (w *wsConn) readStatus() (Status, error) {
	for range maxSkippedMessages + 1 {
		msg, err := w.readMessage()
		if err != nil {
			return Status{State: StateUnknown}, err
		}
		st, err := parseFAHMessage(msg)
		if errors.Is(err, errNotSnapshot) {
			continue
		}
		return st, err
	}
	return Status{State: StateUnknown}, errors.New("no state snapshot received")
}

// closeGracefully sends a close frame and briefly drains until the server's
// close frame, so the command is not lost to a TCP reset. Errors are ignored:
// the call already succeeded.
func (w *wsConn) closeGracefully(deadline time.Time) {
	if err := w.writeFrame(opClose, []byte{0x03, 0xE8}); err != nil { // 1000: normal closure
		return
	}
	drainUntil := time.Now().Add(closeDrainTimeout)
	if deadline.Before(drainUntil) {
		drainUntil = deadline
	}
	_ = w.conn.SetReadDeadline(drainUntil)
	for range maxSkippedMessages {
		if _, err := w.readMessage(); err != nil {
			return
		}
	}
}

// fahSnapshot holds the fields EcoFold reads from the FAH v8 state snapshot.
// v8.3+ keeps the paused flag per resource group; older clients keep it in
// the top-level config.
type fahSnapshot struct {
	Config *fahConfig          `json:"config"`
	Groups map[string]fahGroup `json:"groups"`
	// Units are decoded one by one so that a unit with unexpected field types
	// is skipped instead of failing the snapshot and blocking a pause.
	Units []json.RawMessage `json:"units"`
}

type fahConfig struct {
	Paused bool `json:"paused"`
}

type fahGroup struct {
	Config fahConfig `json:"config"`
}

type fahUnit struct {
	State       string `json:"state"`
	PauseReason string `json:"pause_reason"`
	Assignment  struct {
		Project json.Number `json:"project"`
	} `json:"assignment"`
}

// projectID returns the unit's project as a positive int, accepting JSON
// numbers written as integers or integral floats (18626, 18626.0) and numeric
// strings ("18626"). Anything else yields 0.
func (u fahUnit) projectID() int {
	f, err := strconv.ParseFloat(u.Assignment.Project.String(), 64)
	if err != nil || f < 1 || f > math.MaxInt32 || f != math.Trunc(f) {
		return 0
	}
	return int(f)
}

// parseFAHMessage parses a FAH v8 WebSocket message into a Status. Only the
// initial state snapshot (a JSON object) carries the full state; incremental
// updates (JSON arrays) and other values return errNotSnapshot.
func parseFAHMessage(raw []byte) (Status, error) {
	unknown := Status{State: StateUnknown}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != '{' {
		return unknown, errNotSnapshot
	}
	var snap fahSnapshot
	if err := json.Unmarshal([]byte(trimmed), &snap); err != nil {
		return unknown, fmt.Errorf("invalid state snapshot: %w", err)
	}
	if snap.Config == nil && len(snap.Groups) == 0 {
		return unknown, errors.New("state snapshot has no config or groups")
	}

	// Paused means paused everywhere, matching the Web Control's is_paused().
	paused := true
	if len(snap.Groups) > 0 {
		for _, g := range snap.Groups {
			paused = paused && g.Config.Paused
		}
	} else {
		paused = snap.Config.Paused
	}

	project := 0
	for _, raw := range snap.Units {
		var u fahUnit
		if json.Unmarshal(raw, &u) != nil {
			continue
		}
		if id := u.projectID(); u.State == "RUN" && u.PauseReason == "" && id > 0 {
			project = id
			break
		}
	}

	switch {
	case paused:
		return Status{Paused: true, State: StatePaused}, nil
	case project != 0:
		return Status{ActiveProjectID: project, State: StateFolding}, nil
	default:
		return Status{State: StateIdle}, nil
	}
}
