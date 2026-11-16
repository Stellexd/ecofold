// Package ipc is the local control channel between the daemon and its
// unprivileged clients (the GUI): a Unix domain socket carrying one JSON
// message per line.
//
// The daemon runs as root on Linux, so a bug here is a root bug. The package
// therefore depends only on the standard library and golang.org/x/sys, never
// on internal/daemon: the daemon plugs in through Handler. See README.md for
// the wire protocol and the security model.
package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

const (
	// Version is the only protocol version the server accepts.
	Version = 1
	// MaxMessage is the largest request or response, in bytes, not counting
	// the terminating newline.
	MaxMessage = 64 << 10
	// MaxIDLen is the longest request id, in bytes.
	MaxIDLen = 64
)

// Commands.
const (
	CmdStatus            = "status"
	CmdForcePause        = "force_pause"
	CmdForceResume       = "force_resume"
	CmdSetPauseProcesses = "set_pause_processes"
	CmdSetEcoWatts       = "set_eco_watts"
)

// Error codes.
const (
	CodeBadRequest     = "bad_request"
	CodeUnknownCmd     = "unknown_cmd"
	CodeTooLarge       = "too_large"
	CodeUnsupported    = "unsupported"
	CodeOutOfRange     = "out_of_range"
	CodeCapFailed      = "cap_failed"
	CodeGameActive     = "game_active"
	CodeOnBattery      = "on_battery"
	CodeFAHUnavailable = "fah_unavailable"
	CodeBusy           = "busy"
	CodeStopping       = "stopping"
	CodeInternal       = "internal"
)

// Request is one client message.
type Request struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Cmd  string          `json:"cmd"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response answers one Request. Exactly one of Data and Error is set. The
// server fills V and ID.
type Response struct {
	V     int             `json:"v"`
	ID    string          `json:"id"`
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *Error          `json:"error,omitempty"`
}

// Error is the failure half of a Response.
type Error struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

// Handler executes a validated request: the version, the id length and the
// command name were already checked. Handle is called from one goroutine per
// connection, so it must be safe for concurrent use, and it must return
// promptly: the connection is blocked until it does.
type Handler interface {
	Handle(ctx context.Context, req Request) Response
}

// OK builds a success response carrying data as JSON.
func OK(data any) Response {
	raw, err := json.Marshal(data)
	if err != nil {
		return Fail(CodeInternal, "could not encode the response")
	}
	return Response{OK: true, Data: raw}
}

// Fail builds an error response.
func Fail(code, msg string) Response {
	return Response{Error: &Error{Code: code, Msg: msg}}
}

// DecodeArgs decodes a request's args into v, rejecting unknown fields and
// trailing data. Missing or null args leave v untouched.
func DecodeArgs(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return decodeStrict(raw, v)
}

func knownCmd(cmd string) bool {
	switch cmd {
	case CmdStatus, CmdForcePause, CmdForceResume, CmdSetPauseProcesses, CmdSetEcoWatts:
		return true
	}
	return false
}

var errTooLarge = errors.New("ipc: message larger than MaxMessage")

// readMessage returns the next line without its newline. It never buffers
// more than limit bytes of one message: a longer line returns errTooLarge
// with the rest of the line unread, so the caller must drop the connection.
//
// The limit is enforced here and not with io.LimitReader because the
// connection is persistent: the budget applies to each message, not to the
// stream.
func readMessage(r *bufio.Reader, limit int) ([]byte, error) {
	var msg []byte
	for {
		// Block for at least one byte, then take whatever has arrived: the
		// limit trips as soon as it is exceeded, without waiting for a
		// newline or a full buffer.
		if _, err := r.Peek(1); err != nil {
			return nil, err
		}
		buf, _ := r.Peek(r.Buffered())
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			if len(msg)+i > limit {
				return nil, errTooLarge
			}
			msg = append(msg, buf[:i]...)
			_, _ = r.Discard(i + 1)
			return msg, nil
		}
		if len(msg)+len(buf) > limit {
			return nil, errTooLarge
		}
		msg = append(msg, buf...)
		_, _ = r.Discard(len(buf))
	}
}

// decodeStrict decodes exactly one JSON value into v.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("ipc: trailing data after the JSON value")
	}
	return nil
}

// decodeRequest parses and validates one request line. On failure it returns
// the id to echo, when one could be read, and the error to send back.
func decodeRequest(line []byte) (Request, *Error) {
	var req Request
	if err := decodeStrict(line, &req); err != nil {
		return Request{}, &Error{Code: CodeBadRequest, Msg: "malformed request"}
	}
	if len(req.ID) > MaxIDLen {
		return Request{}, &Error{Code: CodeBadRequest, Msg: "id longer than 64 bytes"}
	}
	if req.V != Version {
		return Request{ID: req.ID}, &Error{Code: CodeBadRequest, Msg: "unsupported protocol version"}
	}
	if !knownCmd(req.Cmd) {
		return Request{ID: req.ID}, &Error{Code: CodeUnknownCmd, Msg: "unknown command"}
	}
	return req, nil
}
