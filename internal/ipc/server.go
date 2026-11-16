package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	// defaultMaxConns bounds concurrent connections; one more is closed on
	// accept.
	defaultMaxConns = 32
	// defaultIOTimeout bounds each read (a whole request, or an idle wait
	// between requests) and each write.
	defaultIOTimeout = 2 * time.Second
	// probeTimeout bounds the liveness check of a socket found at the path.
	probeTimeout = 250 * time.Millisecond
	// rejectLogEvery rate-limits the warnings about refused peers.
	rejectLogEvery = 5 * time.Second
)

// ErrAlreadyRunning is returned by Listen when another process is accepting
// connections on the socket.
var ErrAlreadyRunning = errors.New("ipc: another daemon is already running on this socket")

// PeerCred identifies the process on the other end of a connection, as
// reported by the kernel.
type PeerCred struct {
	UID uint32
	GID uint32
	PID int32
}

// Config configures Listen.
type Config struct {
	// Path is the socket. Its directory is created when missing; it must be
	// owned by the daemon's user and must not be a symlink.
	Path string
	// Group names the group allowed to connect besides root and the daemon's
	// user (Linux only). Empty, or a group that does not exist, leaves the
	// socket restricted to its owner.
	Group string
	// Handler executes the requests. Required.
	Handler Handler
	// Logger receives the server's logs. Default: slog.Default().
	Logger *slog.Logger
}

// Server accepts connections on a Unix domain socket and dispatches their
// requests to a Handler.
type Server struct {
	path    string
	ln      *net.UnixListener
	handler Handler
	log     *slog.Logger

	// authorize decides whether a peer may talk to the daemon. Tests replace
	// it before Serve.
	authorize func(PeerCred) bool
	ioTimeout time.Duration
	sem       chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
	once   sync.Once

	rejects rateLimit
}

// umaskMu serializes the temporary umask change in Listen: the umask is
// process-wide.
var umaskMu sync.Mutex

// Listen prepares the socket directory, replaces a stale socket and starts
// listening. Ownership and mode are final when it returns, before any
// connection is accepted. Call Serve next, and Shutdown to remove the socket.
//
// Listen changes the process umask for the duration of bind(2), so it must
// not run while other goroutines create files.
func Listen(cfg Config) (*Server, error) {
	if cfg.Handler == nil {
		return nil, errors.New("ipc: Handler is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	path := cfg.Path
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("ipc: socket path %q is not absolute", path)
	}
	if len(path) >= maxPathLen {
		return nil, fmt.Errorf("ipc: socket path is %d bytes, the limit on this platform is %d", len(path), maxPathLen-1)
	}

	gid, hasGroup := resolveGroup(cfg.Group, log)
	if err := prepareDir(filepath.Dir(path), gid, hasGroup, log); err != nil {
		return nil, err
	}
	if err := removeStale(path); err != nil {
		return nil, err
	}

	umaskMu.Lock()
	old := syscall.Umask(0o177)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	syscall.Umask(old)
	umaskMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("ipc: listen: %w", err)
	}

	if err := protectSocket(path, gid, hasGroup, log); err != nil {
		_ = ln.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		path:      path,
		ln:        ln,
		handler:   cfg.Handler,
		log:       log,
		authorize: newAuthorizer(gid, hasGroup),
		ioTimeout: defaultIOTimeout,
		sem:       make(chan struct{}, defaultMaxConns),
		ctx:       ctx,
		cancel:    cancel,
		conns:     map[net.Conn]struct{}{},
		rejects:   rateLimit{every: rejectLogEvery},
	}, nil
}

// resolveGroup maps the configured group to a gid. Any failure is fail-closed:
// the socket stays owner-only and the daemon keeps running.
func resolveGroup(name string, log *slog.Logger) (gid uint32, ok bool) {
	if name == "" {
		return 0, false
	}
	if !groupSupported {
		log.Warn("ipc: a socket group is not supported on this platform; the socket stays owner-only", "group", name)
		return 0, false
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		log.Warn("ipc: socket group not found; the socket is restricted to root until the group exists and the daemon restarts",
			"group", name, "err", err)
		return 0, false
	}
	n, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		log.Warn("ipc: socket group has a non-numeric id; the socket stays owner-only", "group", name, "gid", g.Gid)
		return 0, false
	}
	return uint32(n), true
}

// prepareDir creates or validates the socket directory. An existing directory
// must be a real directory owned by the daemon's user: anything else could be
// controlled by someone else, and the daemon refuses to put its socket there.
func prepareDir(dir string, gid uint32, hasGroup bool, log *slog.Logger) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(dir, dirMode); err != nil {
			return fmt.Errorf("ipc: socket directory: %w", err)
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("ipc: socket directory: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("ipc: socket directory %s is not a directory (mode %s)", dir, fi.Mode())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("ipc: socket directory %s: owner unknown", dir)
	}
	if euid := uint32(os.Geteuid()); st.Uid != euid {
		return fmt.Errorf("ipc: socket directory %s is owned by uid %d, not by the daemon's uid %d", dir, st.Uid, euid)
	}
	if hasGroup && st.Gid != gid {
		if err := os.Lchown(dir, -1, int(gid)); err != nil {
			// Fail-closed: without the group on the directory its members
			// cannot reach the socket.
			log.Warn("ipc: could not give the socket directory to the socket group; only root can connect",
				"dir", dir, "gid", gid, "err", err)
		}
	}
	if fi.Mode().Perm() != dirMode {
		// Keep a setgid bit: the systemd unit sets it so the socket inherits
		// the group without CAP_CHOWN.
		if err := os.Chmod(dir, dirMode|fi.Mode()&os.ModeSetgid); err != nil {
			return fmt.Errorf("ipc: socket directory: %w", err)
		}
	}
	return nil
}

// removeStale clears the socket path for a new listener. Only a socket nobody
// is listening on is removed; a live daemon, a symlink, a regular file or a
// directory is an error and is left untouched.
func removeStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ipc: socket path: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("ipc: %s exists and is not a socket (mode %s); refusing to replace it", path, fi.Mode())
	}
	c, err := net.DialTimeout("unix", path, probeTimeout)
	if err == nil {
		_ = c.Close()
		return fmt.Errorf("%w: %s", ErrAlreadyRunning, path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("ipc: cannot tell whether the socket at %s is in use: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("ipc: remove stale socket: %w", err)
	}
	return nil
}

// protectSocket sets the final owner group and mode. bind(2) created the
// socket 0600 under the temporary umask; it is opened to the group only once
// the group really owns it.
func protectSocket(path string, gid uint32, hasGroup bool, log *slog.Logger) error {
	mode := os.FileMode(0o600)
	if hasGroup {
		fi, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("ipc: socket: %w", err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case ok && st.Gid == gid:
			mode = 0o660
		default:
			if err := os.Lchown(path, -1, int(gid)); err != nil {
				log.Warn("ipc: could not give the socket to the socket group; only root can connect",
					"path", path, "gid", gid, "err", err)
			} else {
				mode = 0o660
			}
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("ipc: socket: %w", err)
	}
	return nil
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Serve accepts connections until Shutdown. It always returns nil after
// Shutdown; accept errors are retried with a backoff, never fatal.
func (s *Server) Serve() error {
	backoff := 5 * time.Millisecond
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			if s.isClosed() {
				return nil
			}
			s.log.Warn("ipc: accept failed; retrying", "err", err, "in", backoff)
			select {
			case <-s.ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond

		select {
		case s.sem <- struct{}{}:
		default:
			if n, ok := s.rejects.allow(); ok {
				s.log.Warn("ipc: connection limit reached; connection closed", "limit", cap(s.sem), "suppressed", n)
			}
			_ = c.Close()
			continue
		}
		if !s.track(c) {
			<-s.sem
			_ = c.Close()
			continue
		}
		go s.serveConn(c)
	}
}

// Shutdown stops accepting, closes every connection, waits for the handlers
// until ctx is done and removes the socket. It is idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.cancel()
		// Close unlinks the socket file the listener created.
		_ = s.ln.Close()
	})

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ipc: shutdown: %w", ctx.Err())
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.wg.Done()
}

// serveConn authenticates the peer and then answers its requests one at a
// time until it disconnects, idles past the timeout or misbehaves. A panic
// closes this connection only.
func (s *Server) serveConn(c *net.UnixConn) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("ipc: panic while serving a connection", "panic", r, "stack", string(debug.Stack()))
		}
		_ = c.Close()
		s.untrack(c)
		<-s.sem
	}()

	// Authenticate before reading a single byte.
	cred, err := peerCred(c)
	if err != nil {
		if n, ok := s.rejects.allow(); ok {
			s.log.Warn("ipc: could not read peer credentials; connection closed", "err", err, "suppressed", n)
		}
		return
	}
	if !s.authorize(cred) {
		if n, ok := s.rejects.allow(); ok {
			s.log.Warn("ipc: peer not authorized; connection closed",
				"uid", cred.UID, "gid", cred.GID, "pid", cred.PID, "suppressed", n)
		}
		return
	}

	br := bufio.NewReader(c)
	for {
		// One deadline covers the whole message, so trickling bytes does not
		// extend it.
		if err := c.SetReadDeadline(time.Now().Add(s.ioTimeout)); err != nil {
			return
		}
		line, err := readMessage(br, MaxMessage)
		if errors.Is(err, errTooLarge) {
			_ = s.write(c, Response{Error: &Error{Code: CodeTooLarge, Msg: "request larger than 65536 bytes"}})
			return
		}
		if err != nil {
			return
		}

		req, bad := decodeRequest(line)
		var resp Response
		if bad != nil {
			resp = Response{Error: bad}
		} else {
			resp = s.handler.Handle(s.ctx, req)
		}
		resp.ID = req.ID
		if err := s.write(c, resp); err != nil {
			return
		}
	}
}

// write sends one response line within the write timeout.
func (s *Server) write(c net.Conn, resp Response) error {
	resp.V = Version
	resp.OK = resp.Error == nil
	if !resp.OK {
		resp.Data = nil
	}
	raw, err := json.Marshal(resp)
	if err != nil || len(raw) > MaxMessage {
		raw, _ = json.Marshal(Response{V: Version, ID: resp.ID,
			Error: &Error{Code: CodeInternal, Msg: "response could not be encoded"}})
	}
	if err := c.SetWriteDeadline(time.Now().Add(s.ioTimeout)); err != nil {
		return err
	}
	_, err = c.Write(append(raw, '\n'))
	return err
}

// rateLimit lets one event through per period and counts the rest.
type rateLimit struct {
	every time.Duration

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// allow reports whether to log now, and how many events were suppressed since
// the last log.
func (r *rateLimit) allow() (suppressed int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.last.IsZero() && now.Sub(r.last) < r.every {
		r.suppressed++
		return 0, false
	}
	suppressed, r.suppressed, r.last = r.suppressed, 0, now
	return suppressed, true
}
