package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"syscall"
	"time"

	"github.com/Stellexd/ecofold/internal/ipc"
)

const (
	// DefaultPoll is how often the status is refreshed.
	DefaultPoll = 2 * time.Second
	// DefaultTimeout bounds one request, a status poll or a user action.
	DefaultTimeout = 2 * time.Second
)

// Config configures a Controller.
type Config struct {
	// SocketPath is the daemon's socket. Required.
	SocketPath string
	// Poll is the status refresh period. Default: DefaultPoll.
	Poll time.Duration
	// Timeout bounds every request. Default: DefaultTimeout.
	Timeout time.Duration
	// OnChange is called with the new view after every change, from the
	// goroutine that caused it. It must not block; a UI should hand the view
	// to its own thread.
	OnChange func(View)
	// Projects fetches the active Folding@home project's metadata for
	// View.Project. Nil disables it: the view only carries the project id.
	Projects *ProjectClient
}

// Controller keeps a View in sync with the daemon and sends the user's
// actions. It never fails for good: while the daemon is down, restarting or
// refusing the connection, the view says so and the next poll tries again.
//
// A Controller is safe for concurrent use. Its methods block for at most
// Timeout, so a UI calls them from a goroutine, not from its event thread.
type Controller struct {
	path     string
	poll     time.Duration
	timeout  time.Duration
	onChange func(View)
	projects *ProjectClient

	// clientMu guards the lazily dialed client; mu guards the view and
	// lookingUp, the project id being fetched (0: none).
	clientMu  sync.Mutex
	client    *ipc.Client
	mu        sync.Mutex
	view      View
	lookingUp int
}

// New returns a Controller. Nothing is dialed until Run or the first call.
func New(cfg Config) *Controller {
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	c := &Controller{path: cfg.SocketPath, poll: cfg.Poll, timeout: cfg.Timeout, onChange: cfg.OnChange, projects: cfg.Projects}
	c.view.Offline, c.view.OfflineHint = "Connecting…", ""
	return c
}

// View returns the current view.
func (c *Controller) View() View {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view
}

// Run refreshes the status now and then every Poll until ctx is done, then
// closes the connection.
func (c *Controller) Run(ctx context.Context) {
	defer c.dropClient()
	t := time.NewTicker(c.poll)
	defer t.Stop()
	for {
		c.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Refresh asks for the status once and updates the view: connected with the
// new status, or disconnected with the reason.
func (c *Controller) Refresh(ctx context.Context) {
	st, err := c.call(ctx, ipc.CmdStatus, nil)
	if ctx.Err() != nil {
		return // shutting down: not a daemon failure
	}
	c.update(func(v *View) {
		if err != nil {
			v.Connected = false
			v.Offline, v.OfflineHint = describeOffline(err)
			return
		}
		v.Connected, v.Offline, v.OfflineHint = true, "", ""
		c.setStatus(v, st)
	})
	c.lookupProject(ctx)
}

// setStatus stores st in v and keeps v.Project in step with the active
// project: it is dropped when the project changes and filled from the cache
// when it can be.
func (c *Controller) setStatus(v *View, st Status) {
	v.Status = st
	id := st.FAH.ActiveProjectID
	if v.Project.ID == id {
		return
	}
	v.Project = Project{}
	if c.projects != nil && id > 0 {
		v.Project, _ = c.projects.Cached(id)
	}
}

// lookupProject fetches the active project's metadata in the background when
// the view does not have it yet, one lookup at a time. A failure changes
// nothing: the view keeps the bare project id and a later poll tries again.
func (c *Controller) lookupProject(ctx context.Context) {
	if c.projects == nil {
		return
	}
	c.mu.Lock()
	id := c.view.Status.FAH.ActiveProjectID
	if !c.view.Connected || id <= 0 || c.view.Project.ID == id || c.lookingUp != 0 {
		c.mu.Unlock()
		return
	}
	c.lookingUp = id
	c.mu.Unlock()

	go func() {
		p, err := c.projects.Lookup(ctx, id)
		c.mu.Lock()
		c.lookingUp = 0
		stale := err != nil || c.view.Status.FAH.ActiveProjectID != id
		c.mu.Unlock()
		if stale || ctx.Err() != nil {
			return
		}
		c.update(func(v *View) {
			if v.Status.FAH.ActiveProjectID == id {
				v.Project = p
			}
		})
	}()
}

// Pause asks the daemon to hold folding paused until Resume.
func (c *Controller) Pause(ctx context.Context) {
	c.act(ctx, ipc.CmdForcePause, nil, "Folding paused")
}

// Resume clears the manual pause and resumes folding. The daemon may refuse
// (a game is running, on battery, the power limit failed): the view's
// message says why.
func (c *Controller) Resume(ctx context.Context) {
	c.act(ctx, ipc.CmdForceResume, nil, "Folding resumed")
}

// Toggle runs the quick action the view advertises with ToggleLabel.
func (c *Controller) Toggle(ctx context.Context) {
	if c.View().WantsResume() {
		c.Resume(ctx)
		return
	}
	c.Pause(ctx)
}

// SetEcoWatts changes the power limit used while folding. A value outside
// the device range is refused here, without a request; the daemon checks it
// again.
func (c *Controller) SetEcoWatts(ctx context.Context, watts int) {
	v := c.View()
	if v.Connected {
		s := v.Status
		if !s.PowerCapable {
			c.setMessage("This GPU has no power limit control", true)
			return
		}
		if watts < s.MinPowerLimitW || watts > s.MaxPowerLimitW {
			c.setMessage(fmt.Sprintf("Eco limit must be between %d and %d W", s.MinPowerLimitW, s.MaxPowerLimitW), true)
			return
		}
	}
	c.act(ctx, ipc.CmdSetEcoWatts, map[string]int{"watts": watts}, fmt.Sprintf("Eco limit set to %d W", watts))
}

// SetPauseProcesses replaces the pause list with what the user typed:
// patterns separated by commas or new lines. Empty text clears the list.
func (c *Controller) SetPauseProcesses(ctx context.Context, text string) {
	patterns := ParsePauseList(text)
	msg := "Pause list saved"
	if len(patterns) == 0 {
		msg = "Pause list cleared"
	}
	c.act(ctx, ipc.CmdSetPauseProcesses, map[string][]string{"patterns": patterns}, msg)
}

// act sends a state-changing command and reports the outcome in the view's
// message. On success the response's status replaces the view's.
func (c *Controller) act(ctx context.Context, cmd string, args any, okMsg string) {
	st, err := c.call(ctx, cmd, args)
	if ctx.Err() != nil {
		return
	}

	var remote *ipc.RemoteError
	switch {
	case err == nil:
		c.update(func(v *View) {
			v.Connected, v.Offline, v.OfflineHint = true, "", ""
			c.setStatus(v, st)
			v.Message, v.MessageIsError, v.MessageAt = okMsg, false, time.Now()
		})
		c.lookupProject(ctx)
	case errors.As(err, &remote):
		// The daemon answered, so it is up. Some refusals still change its
		// state (force_resume clears the manual pause), so read it again.
		c.setMessage(describeRemote(remote), true)
		c.Refresh(ctx)
	default:
		offline, hint := describeOffline(err)
		msg := "Not sent: " + lowerFirst(offline)
		if errors.Is(err, context.DeadlineExceeded) {
			// The request may have arrived: do not claim it did not.
			msg = "No answer from the daemon in time. Check the state before trying again."
		}
		c.update(func(v *View) {
			v.Connected, v.Offline, v.OfflineHint = false, offline, hint
			v.Message, v.MessageIsError, v.MessageAt = msg, true, time.Now()
		})
	}
}

func (c *Controller) setMessage(msg string, isErr bool) {
	c.update(func(v *View) {
		v.Message, v.MessageIsError, v.MessageAt = msg, isErr, time.Now()
	})
}

// update changes the view under the lock and then notifies, outside it.
func (c *Controller) update(change func(*View)) {
	c.mu.Lock()
	change(&c.view)
	v := c.view
	c.mu.Unlock()
	if c.onChange != nil {
		c.onChange(v)
	}
}

// call sends one request within Timeout, dialing first if needed. After a
// transport error the connection is dropped, so the next call starts clean.
func (c *Controller) call(ctx context.Context, cmd string, args any) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if c.client == nil {
		cl, err := ipc.Dial(ctx, c.path)
		if err != nil {
			return Status{}, err
		}
		c.client = cl
	}
	raw, err := c.client.Call(ctx, cmd, args)
	if err != nil {
		var remote *ipc.RemoteError
		if !errors.As(err, &remote) {
			_ = c.client.Close()
			c.client = nil
		}
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return Status{}, fmt.Errorf("gui: unreadable status from the daemon: %w", err)
	}
	return st, nil
}

func (c *Controller) dropClient() {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if c.client != nil {
		_ = c.client.Close()
		c.client = nil
	}
}

// describeOffline turns a transport error into what the user sees.
func describeOffline(err error) (reason, hint string) {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "Access denied", `Add your user to the "ecofold" group and log in again.`
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		// The daemon accepted the connection and hung up without a word:
		// that is how it refuses a peer it does not authorize.
		return "Access denied", "The daemon closed the connection. It only accepts its own user and the socket group."
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return "Daemon not running", "Start ecofold-daemon; this window reconnects by itself."
	case errors.Is(err, context.DeadlineExceeded):
		return "Daemon not responding", "The daemon did not answer in time."
	default:
		return "Daemon not reachable", err.Error()
	}
}

// describeRemote turns a daemon error code into a sentence for the status
// bar.
func describeRemote(e *ipc.RemoteError) string {
	switch e.Code {
	case ipc.CodeGameActive:
		return "Not resumed: a game or GPU load is active. Folding resumes after the cooldown."
	case ipc.CodeOnBattery:
		return "Not resumed: on battery. Folding resumes when the battery allows it."
	case ipc.CodeThermal:
		return "Not resumed: the Mac is under thermal pressure. Folding resumes after it cools down."
	case ipc.CodeCapFailed:
		return "The GPU rejected the power limit, so nothing was changed."
	case ipc.CodeFAHUnavailable:
		return "Folding@home did not respond. Check that the FAH client is running."
	case ipc.CodeOutOfRange:
		return "That power limit is outside what this GPU allows."
	case ipc.CodeUnsupported:
		return "Not supported on this system."
	case ipc.CodeBadRequest:
		return "Rejected: " + e.Msg
	case ipc.CodeBusy:
		return "The daemon is busy. Try again in a moment."
	case ipc.CodeStopping:
		return "The daemon is shutting down."
	default:
		return fmt.Sprintf("Daemon error (%s): %s", e.Code, e.Msg)
	}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}
