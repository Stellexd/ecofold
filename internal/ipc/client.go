package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// defaultCallTimeout bounds a Call whose context has no deadline.
const defaultCallTimeout = 5 * time.Second

// RemoteError is an error response from the daemon.
type RemoteError struct {
	Code string
	Msg  string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("ipc: daemon error %s: %s", e.Code, e.Msg)
}

// Client talks to the daemon over its socket. It is safe for concurrent use;
// calls are sent one at a time.
//
// The daemon closes connections that idle for 2 s. Call redials when it finds
// the connection closed before anything was sent, so a long-lived Client keeps
// working across idle periods and daemon restarts.
type Client struct {
	path string

	mu   sync.Mutex
	conn net.Conn
	br   *bufio.Reader
	seq  uint64
}

// Dial connects to the daemon's socket at path.
func Dial(ctx context.Context, path string) (*Client, error) {
	c := &Client{path: path}
	if err := c.dial(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) dial(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return fmt.Errorf("ipc: dial: %w", err)
	}
	c.conn, c.br = conn, bufio.NewReader(conn)
	return nil
}

// Close closes the connection. A later Call redials.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.drop()
}

func (c *Client) drop() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn, c.br = nil, nil
	return err
}

// Call sends one command and waits for its response, honoring ctx's deadline
// and cancellation. args is encoded as JSON; nil sends no args. A failure
// reported by the daemon is returned as *RemoteError.
//
// After a transport error the outcome of a state-changing command is unknown:
// read the status before retrying.
func (c *Client) Call(ctx context.Context, cmd string, args any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seq++
	req := Request{V: Version, ID: strconv.FormatUint(c.seq, 10), Cmd: cmd}
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("ipc: encode args: %w", err)
		}
		req.Args = raw
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("ipc: encode request: %w", err)
	}
	if len(line) > MaxMessage {
		return nil, fmt.Errorf("ipc: request is %d bytes, the limit is %d", len(line), MaxMessage)
	}
	line = append(line, '\n')

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCallTimeout)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()

	// A failed first write means the daemon closed the idle connection and
	// received nothing, so one retry on a fresh connection is safe.
	for attempt := 0; ; attempt++ {
		if c.conn == nil {
			if err := c.dial(ctx); err != nil {
				return nil, err
			}
		}
		_ = c.conn.SetDeadline(deadline)
		if _, err = c.conn.Write(line); err == nil {
			break
		}
		_ = c.drop()
		if attempt > 0 || ctx.Err() != nil {
			return nil, c.wrap(ctx, "write", err)
		}
	}

	// Unblock the read when ctx is canceled before its deadline.
	conn := c.conn
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	raw, err := readMessage(c.br, MaxMessage)
	if err != nil {
		_ = c.drop()
		return nil, c.wrap(ctx, "read", err)
	}
	var resp Response
	if err := decodeStrict(raw, &resp); err != nil {
		_ = c.drop()
		return nil, fmt.Errorf("ipc: malformed response: %w", err)
	}
	if resp.V != Version || resp.ID != req.ID {
		_ = c.drop()
		return nil, fmt.Errorf("ipc: response does not match the request (v=%d id=%q, want id %q)", resp.V, resp.ID, req.ID)
	}
	if !resp.OK {
		if resp.Error == nil {
			_ = c.drop()
			return nil, errors.New("ipc: error response without an error")
		}
		return nil, &RemoteError{Code: resp.Error.Code, Msg: resp.Error.Msg}
	}
	return resp.Data, nil
}

// wrap prefers the context's error over the i/o error it caused.
func (c *Client) wrap(ctx context.Context, op string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("ipc: %s: %w", op, cerr)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("ipc: %s: %w", op, context.DeadlineExceeded)
	}
	return fmt.Errorf("ipc: %s: %w", op, err)
}
