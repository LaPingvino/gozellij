package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Server serves a fabric over a unix socket.
type Server struct {
	fab  *fabric.Fabric
	log  *slog.Logger
	path string

	ln net.Listener

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool

	wg sync.WaitGroup
}

// ErrDaemonRunning is returned when another daemon already owns the socket.
var ErrDaemonRunning = errors.New("another gozellij daemon is already running")

// Listen binds the socket and returns a server ready to Serve.
//
// A socket file left behind by a daemon that died is removed, but only after checking that nothing
// is listening on it: deleting a live daemon's socket would leave it running and unreachable,
// which is a much worse state than refusing to start.
func Listen(path string, fab *fabric.Fabric, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := CheckSocketPath(path); err != nil {
		return nil, err
	}

	dir := filepath.Dir(path)
	// 0700: the socket controls every process this fabric runs, so it is nobody else's business.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}

	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	// Belt and braces: the directory is already 0700, but a socket inheriting a permissive
	// umask would be reachable by anyone who can reach the directory.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("securing %s: %w", path, err)
	}

	return &Server{
		fab:   fab,
		log:   log,
		path:  path,
		ln:    ln,
		conns: make(map[net.Conn]struct{}),
	}, nil
}

// clearStaleSocket removes a socket file whose daemon is gone, and refuses when one is alive.
func clearStaleSocket(path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("checking %s: %w", path, err)
	}

	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("%w (socket %s is live)", ErrDaemonRunning, path)
	}
	// Nothing accepted: either the daemon is gone (ECONNREFUSED) or the file is not a socket.
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT) {
		// Be careful here. An unexpected error is not proof the daemon is dead, and removing a
		// live daemon's socket would leave it running and unreachable.
		return fmt.Errorf("cannot tell whether a daemon owns %s (%v); "+
			"remove it by hand if you are sure nothing is running", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale socket %s: %w", path, err)
	}
	return nil
}

// Addr is the socket path being served.
func (s *Server) Addr() string { return s.path }

// Serve accepts connections until the server is closed. It returns nil on a clean shutdown.
func (s *Server) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("accepting on %s: %w", s.path, err)
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.dropConn(conn)
			s.handle(conn)
		}()
	}
}

func (s *Server) dropConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	conn.Close()
}

// Close stops accepting, hangs up on every client and removes the socket. It does not touch the
// fabric: stopping the services is a separate decision, and a daemon restarting to pick up a new
// binary must not take the processes down with it.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	err := s.ln.Close()
	for _, c := range conns {
		c.Close()
	}
	s.wg.Wait()

	// The listener usually unlinks the socket; make sure, so the next start does not have to
	// reason about a stale file.
	if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		s.log.Warn("could not remove socket", "path", s.path, "err", rmErr)
	}
	return err
}

// handle serves one connection until it hangs up.
func (s *Server) handle(conn net.Conn) {
	r := ipc.NewReader(conn)
	w := ipc.NewWriter(conn)

	for {
		kind, payload, err := r.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				// A client that goes away mid-frame is worth a line in the log. Silence
				// here is how a protocol bug becomes "it sometimes does nothing".
				s.log.Debug("connection ended", "err", err)
			}
			return
		}

		if kind != ipc.KindRequest {
			// Data frames only make sense inside an attach, which owns the connection for
			// its duration; anything else is a client bug and is said out loud.
			_ = w.WriteJSON(ipc.KindResponse, ipc.Err(0,
				fmt.Errorf("unexpected %s frame: this connection is not attached", kind)))
			continue
		}

		var req ipc.Request
		if err := json.Unmarshal(payload, &req); err != nil {
			_ = w.WriteJSON(ipc.KindResponse, ipc.Err(0, fmt.Errorf("malformed request: %w", err)))
			continue
		}

		resp := s.dispatch(req)
		if err := w.WriteJSON(ipc.KindResponse, resp); err != nil {
			s.log.Debug("could not answer", "op", req.Op, "err", err)
			return
		}
	}
}

// dispatch runs one request. Every path returns a response - including the failures, and including
// operations that do not exist yet.
func (s *Server) dispatch(req ipc.Request) ipc.Response {
	switch req.Op {
	case ipc.OpPing:
		return ipc.OKResponse(req.ID, map[string]string{"pong": "gozellij"})

	case ipc.OpServiceAdd:
		return s.add(req)

	case ipc.OpServiceList:
		return s.list(req)

	case ipc.OpServiceStatus:
		st, err := s.fab.Status(req.Service)
		if err != nil {
			return ipc.Err(req.ID, err)
		}
		return ipc.OKResponse(req.ID, s.statusReply(st))

	case ipc.OpServiceStart:
		if err := s.fab.Start(req.Service); err != nil {
			return ipc.Err(req.ID, err)
		}
		return s.statusAfter(req)

	case ipc.OpServiceStop:
		if err := s.fab.Stop(req.Service); err != nil {
			return ipc.Err(req.ID, err)
		}
		return s.statusAfter(req)

	case ipc.OpServiceRestrt:
		if err := s.fab.Restart(req.Service); err != nil {
			return ipc.Err(req.ID, err)
		}
		return s.statusAfter(req)

	case ipc.OpServiceRemove:
		if err := s.fab.Remove(req.Service); err != nil {
			return ipc.Err(req.ID, err)
		}
		return ipc.OKResponse(req.ID, nil)

	case ipc.OpAttach, ipc.OpResize:
		// Not built yet, and said so plainly with the op named. A daemon that ignores an
		// operation it does not implement is indistinguishable from one that is broken.
		return ipc.Err(req.ID, fmt.Errorf("%q is not implemented yet (Phase 1, in progress)", req.Op))

	default:
		// Naming the op matters: a client from a newer version should learn that this daemon
		// is older, not merely that something went wrong.
		return ipc.Err(req.ID, fmt.Errorf("unknown operation %q - this daemon may be older than your client", req.Op))
	}
}

func (s *Server) add(req ipc.Request) ipc.Response {
	var add ipc.AddRequest
	if len(req.Payload) > 0 {
		if err := json.Unmarshal(req.Payload, &add); err != nil {
			return ipc.Err(req.ID, fmt.Errorf("malformed %s payload: %w", req.Op, err))
		}
	}
	policy, err := fabric.ParseRestartPolicy(add.Restart)
	if err != nil {
		return ipc.Err(req.ID, err)
	}
	svc := fabric.Service{
		Name:    req.Service,
		Command: add.Command,
		Args:    add.Args,
		Dir:     add.Dir,
		Env:     add.Env,
		Restart: policy,
	}
	if err := s.fab.Add(svc, add.Start); err != nil {
		return ipc.Err(req.ID, err)
	}
	return s.statusAfter(req)
}

func (s *Server) list(req ipc.Request) ipc.Response {
	statuses := s.fab.List()
	reply := ipc.ListReply{Services: make([]ipc.StatusReply, 0, len(statuses))}
	for _, st := range statuses {
		reply.Services = append(reply.Services, s.statusReply(st))
	}
	return ipc.OKResponse(req.ID, reply)
}

// SettleWait is how long an operation that starts something waits for the supervisor to get
// somewhere before reporting back.
//
// Without it, `gozellij start web` prints "stopped": true at the instant it was sampled, and
// useless, because the supervisor had not yet spawned anything. Waiting briefly means the answer
// says "running", or "failed" with the reason - which is what the person asking wanted to know.
// Bounded, because a service that takes a while to come up must not hang the command.
const SettleWait = 2 * time.Second

// statusAfter answers with the service's status, so a client that started something can show the
// result without a second round trip.
func (s *Server) statusAfter(req ipc.Request) ipc.Response {
	st, err := s.settledStatus(req.Service)
	if err != nil {
		return ipc.Err(req.ID, err)
	}
	return ipc.OKResponse(req.ID, s.statusReply(st))
}

// settledStatus gives the supervisor a moment to leave StateStopped before reporting.
func (s *Server) settledStatus(name string) (fabric.Status, error) {
	st, err := s.fab.Status(name)
	if err != nil || st.State != fabric.StateStopped {
		return st, err
	}
	// Only wait when the service is *meant* to be running; a deliberate stop is already the
	// final answer and must return immediately.
	if def, derr := s.fab.Definition(name); derr != nil || !def.Enabled {
		return st, err
	}

	deadline := time.Now().Add(SettleWait)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		st, err = s.fab.Status(name)
		if err != nil || st.State != fabric.StateStopped {
			return st, err
		}
	}
	return st, err
}

// statusReply converts the internal status to the wire type, filling in the bits that live in the
// definition rather than in the supervisor.
func (s *Server) statusReply(st fabric.Status) ipc.StatusReply {
	out := ipc.StatusReply{
		Service:     st.Service,
		State:       st.State.String(),
		Pid:         st.Pid,
		StartedAt:   st.StartedAt,
		Restarts:    st.Restarts,
		TotalStarts: st.TotalStarts,
		ExitCode:    st.LastExit.Code,
		ExitSignal:  st.LastExit.Signal,
		HasExited:   st.HasExited,
		NextRestart: st.NextRestart,
		LastError:   st.LastError,
	}
	if def, err := s.fab.Definition(st.Service); err == nil {
		out.Enabled = def.Enabled
		out.Command = def.Command
	}
	return out
}
