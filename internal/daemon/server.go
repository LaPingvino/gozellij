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

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	closed  bool
	version string
	// viewers counts the clients currently attached to each service, and watchers how many of
	// those are read-only.
	//
	// Only the daemon can answer "is anyone watching this?", and it is worth answering: it is
	// the difference between a service nobody has looked at in a week and the one your other
	// terminal is sitting in. A client asking about itself could only ever count to one.
	viewers  map[string]int
	watchers map[string]int

	// upgrades carries an in-band upgrade request out to whoever owns the process (main), since
	// replacing the binary is not something a connection handler can do to itself.
	upgrades chan struct{}

	wg sync.WaitGroup
}

// SetVersion records the daemon's version so clients can see what they are talking to, and what
// they are talking to after an upgrade.
func (s *Server) SetVersion(v string) {
	s.mu.Lock()
	s.version = v
	s.mu.Unlock()
}

// Upgrades fires when a client asks for an in-place upgrade.
func (s *Server) Upgrades() <-chan struct{} { return s.upgrades }

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

	// Before anything touches the socket file: systemd may be holding the listening socket from
	// the last run. Adopting it means a client that connects while the daemon is being restarted
	// is queued rather than refused - and the stale-socket check below would get in the way,
	// because a socket with nobody accepting on it still completes a connect into its backlog and
	// so reads as a live daemon.
	// An exec-in-place hands its socket over directly; only a fresh start asks systemd for one.
	ln, adopted := adoptHandedListener(path, log)
	if !adopted {
		ln, adopted = adoptListener(path, log)
	}
	if !adopted {
		if err := clearStaleSocket(path); err != nil {
			return nil, err
		}
		var err error
		ln, err = net.Listen("unix", path)
		if err != nil {
			return nil, fmt.Errorf("listening on %s: %w", path, err)
		}
		// Belt and braces: the directory is already 0700, but a socket inheriting a permissive
		// umask would be reachable by anyone who can reach the directory.
		if err := os.Chmod(path, 0o600); err != nil {
			ln.Close()
			return nil, fmt.Errorf("securing %s: %w", path, err)
		}
		storeListener(ln, log)
	}

	return &Server{
		fab:      fab,
		log:      log,
		path:     path,
		ln:       ln,
		conns:    make(map[net.Conn]struct{}),
		viewers:  make(map[string]int),
		watchers: make(map[string]int),
		upgrades: make(chan struct{}, 1),
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

		// A following logs request takes the connection over too, for the same reason: after
		// the answer the stream is data frames, not request/response.
		if req.Op == ipc.OpServiceLogs && wantsFollow(req) {
			if err := s.followLogs(conn, r, w, req); err != nil {
				_ = w.WriteJSON(ipc.KindResponse, ipc.Err(req.ID, err))
				s.log.Debug("logs -f ended", "service", req.Service, "err", err)
			}
			return
		}

		// Attach takes the connection over for its lifetime: after this the stream is data
		// frames in both directions, not request/response. It writes its own answer, because
		// it has to succeed *before* the replay starts.
		if req.Op == ipc.OpAttach {
			if err := s.attach(conn, r, w, req); err != nil {
				// The client is gone by now in the ordinary case, so this is best effort -
				// but an attach that failed before it began must still say why.
				_ = w.WriteJSON(ipc.KindResponse, ipc.Err(req.ID, err))
				s.log.Debug("attach ended", "service", req.Service, "err", err)
			}
			return
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
		s.mu.Lock()
		v := s.version
		s.mu.Unlock()
		// Ping carries how the daemon stops things, because only the daemon knows. A client
		// that ran the same detection on itself would be answering about its own cgroup,
		// which has nothing to do with the one the services are in.
		info := map[string]string{"pong": "gozellij", "version": v, "tree_kill": "process group"}
		if cg := s.fab.Cgroups(); cg.Available() {
			info["tree_kill"] = "cgroup"
			info["cgroup"] = cg.Root()
		} else {
			info["tree_kill_why"] = cg.Why()
		}
		return ipc.OKResponse(req.ID, info)

	case ipc.OpServiceLogs:
		return s.logs(req)

	case ipc.OpUpgrade:
		return s.requestUpgrade(req)

	case ipc.OpServiceAdd:
		return s.add(req)

	case ipc.OpServiceEnsure:
		return s.ensure(req)

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

	case ipc.OpServiceRename:
		return s.rename(req)
	case ipc.OpServiceRemove:
		return s.remove(req)

	case ipc.OpAttach:
		// Handled before dispatch, in handle(). Reaching here would mean the routing changed
		// and nobody updated this, so say that rather than pretending.
		return ipc.Err(req.ID, errors.New("internal error: attach reached dispatch"))

	case ipc.OpResize:
		// Resize is only meaningful inside an attach, which owns the stream.
		return ipc.Err(req.ID, errors.New("resize is only valid while attached"))

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
		NoLog:   add.NoLog,
	}
	if err := s.fab.Add(svc, add.Start); err != nil {
		return ipc.Err(req.ID, err)
	}
	return s.statusAfter(req)
}

// MaxLogsBytes caps what one `logs` answer may carry.
//
// A one-shot reply is a single JSON frame, and ipc.MaxFrameSize is 8 MiB - of which base64 eats a
// third before the rest of the envelope. So the daemon picks the ceiling rather than letting a
// client ask for a 32 MiB file and get an unexplained frame error. The reply says Truncated and
// names the file, which is a better answer than failing: the whole log is on disk and the operator
// can read it with anything.
const MaxLogsBytes = 4 << 20 // 4 MiB

// decodeLogs unpacks a logs payload.
func decodeLogs(req ipc.Request) (ipc.LogsRequest, error) {
	var lr ipc.LogsRequest
	if len(req.Payload) == 0 {
		return lr, nil
	}
	if err := json.Unmarshal(req.Payload, &lr); err != nil {
		return lr, fmt.Errorf("malformed %s payload: %w", req.Op, err)
	}
	return lr, nil
}

// wantsFollow reports whether a logs request asked to stream. A payload we cannot read is not a
// follow: dispatch will decode it again and report the parse error properly.
func wantsFollow(req ipc.Request) bool {
	lr, err := decodeLogs(req)
	return err == nil && lr.Follow
}

// ensure makes a service exist and run. See Fabric.Ensure for why it is one operation.
func (s *Server) ensure(req ipc.Request) ipc.Response {
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
		NoLog:   add.NoLog,
	}
	if err := s.fab.Ensure(svc); err != nil {
		return ipc.Err(req.ID, err)
	}
	return s.statusAfter(req)
}

// watching records that a client has attached, and returns the function that records it leaving.
func (s *Server) watching(service string, readOnly bool) func() {
	s.mu.Lock()
	s.viewers[service]++
	if readOnly {
		s.watchers[service]++
	}
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.viewers[service] > 0 {
				s.viewers[service]--
			}
			if s.viewers[service] == 0 {
				delete(s.viewers, service)
			}
			if readOnly {
				if s.watchers[service] > 0 {
					s.watchers[service]--
				}
				if s.watchers[service] == 0 {
					delete(s.watchers, service)
				}
			}
			s.mu.Unlock()
		})
	}
}

// viewerCount reports how many clients are attached to a service.
// exitSignalOf is the signal that ended a service, and nothing for one nobody could observe: the
// marker fabric uses for that is not a signal name and must not be printed as one.
func exitSignalOf(e fabric.Exit) string {
	if e.IsUnknown() {
		return ""
	}
	return e.Signal
}

// watcherCount reports how many of a service's viewers are read-only.
func (s *Server) watcherCount(service string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchers[service]
}

func (s *Server) viewerCount(service string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewers[service]
}

// rename gives a service a new name.
func (s *Server) rename(req ipc.Request) ipc.Response {
	var rr ipc.RenameRequest
	if err := json.Unmarshal(req.Payload, &rr); err != nil {
		return ipc.Err(req.ID, fmt.Errorf("malformed %s payload: %w", req.Op, err))
	}
	if err := s.fab.Rename(req.Service, rr.To); err != nil {
		return ipc.Err(req.ID, err)
	}
	return ipc.OKResponse(req.ID, nil)
}

// remove deletes a service and, unless asked otherwise, its log.
func (s *Server) remove(req ipc.Request) ipc.Response {
	var rr ipc.RemoveRequest
	if len(req.Payload) > 0 {
		if err := json.Unmarshal(req.Payload, &rr); err != nil {
			return ipc.Err(req.ID, fmt.Errorf("malformed %s payload: %w", req.Op, err))
		}
	}

	// Ask before, not after: once the files are gone there is nothing left to name.
	var logs []string
	if !rr.KeepLogs {
		logs = s.fab.LogFiles(req.Service)
	}

	if err := s.fab.Remove(req.Service, rr.KeepLogs); err != nil {
		return ipc.Err(req.ID, err)
	}
	return ipc.OKResponse(req.ID, ipc.RemoveReply{Logs: logs})
}

// logs returns what a service printed, from disk where there is a file for it.
func (s *Server) logs(req ipc.Request) ipc.Response {
	lr, err := decodeLogs(req)
	if err != nil {
		return ipc.Err(req.ID, err)
	}

	max := lr.MaxBytes
	if max <= 0 || max > MaxLogsBytes {
		max = MaxLogsBytes
	}

	tail, err := s.fab.Logs(req.Service, max)
	if err != nil {
		return ipc.Err(req.ID, err)
	}

	reply := ipc.LogsReply{Data: tail.Data, Truncated: tail.Truncated, Path: tail.Path, LogError: tail.Err}
	if st, serr := s.fab.Status(req.Service); serr == nil {
		reply.Running = st.State == fabric.StateRunning
	}
	return ipc.OKResponse(req.ID, reply)
}

// followLogs streams a service's output to a client that is not attached to it.
//
// It is an attach with the keyboard unplugged, and it deliberately reads the live buffer rather
// than tailing the file: the buffer is where the bytes are first, and a follower that tailed the
// file would lag behind by however long the sink took to write. The one-shot form is the one that
// answers questions about the past.
func (s *Server) followLogs(conn net.Conn, r *ipc.Reader, w *ipc.Writer, req ipc.Request) error {
	lr, err := decodeLogs(req)
	if err != nil {
		return err
	}

	out, err := s.fab.Output(req.Service)
	if err != nil {
		return err
	}

	snapshot, sub, err := out.Attach(AttachQueueBytes)
	if err != nil {
		return err
	}

	sess := &attachSession{srv: s, conn: conn, w: w, r: r, service: req.Service}

	// A terminal tailing a service is watching it. The column is called VIEWERS and the question
	// it answers is "is anyone looking at this?" - and someone running `logs -f` in another
	// window is, whatever the name of the command they used.
	//
	// Read-only, and not as a policy: a follower has no way to send anything. It is the same
	// thing `attach -r` asks to be, arrived at from the other direction.
	leaving := s.watching(req.Service, true)
	defer leaving()

	if err := w.WriteJSON(ipc.KindResponse, ipc.OKResponse(req.ID, nil)); err != nil {
		sub.Detach()
		return err
	}

	if lr.MaxBytes > 0 && len(snapshot) > lr.MaxBytes {
		snapshot = snapshot[len(snapshot)-lr.MaxBytes:]
	}
	if len(snapshot) > 0 {
		if err := sess.writeData(snapshot); err != nil {
			sub.Detach()
			return err
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.pumpOutput(sub)
	}()

	err = sess.readUntilHangup()

	// Same order as attach, for the same reason: Detach closes the channel the pump is ranging
	// over, so it must happen before waiting for the pump rather than after it.
	conn.Close()
	sub.Detach()
	<-done
	return err
}

// requestUpgrade hands the request out to the process owner and answers straight away.
//
// The answer has to leave before the exec does, because the exec takes the socket with it. So the
// reply says what is *about* to happen - including which services will not survive - rather than
// reporting afterwards, when there would be no connection left to report on.
func (s *Server) requestUpgrade(req ipc.Request) ipc.Response {
	manifest, problems := PrepareHandover(s.fab)

	reply := ipc.UpgradeReply{Accepted: true, Processes: len(manifest.Processes)}
	for _, p := range problems {
		reply.Problems = append(reply.Problems, p.Error())
	}

	select {
	case s.upgrades <- struct{}{}:
	default:
		// One is already queued. Say so rather than silently doing nothing with the second.
		return ipc.Err(req.ID, errors.New("an upgrade is already in progress"))
	}
	return ipc.OKResponse(req.ID, reply)
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
		ExitSignal:  exitSignalOf(st.LastExit),
		ExitUnknown: st.LastExit.IsUnknown(),
		HasExited:   st.HasExited,
		NextRestart: st.NextRestart,
		LastError:   st.LastError,
		LogError:    st.LogError,
		Viewers:     s.viewerCount(st.Service),
		Watchers:    s.watcherCount(st.Service),
	}
	if def, err := s.fab.Definition(st.Service); err == nil {
		out.Enabled = def.Enabled
		out.Command = def.Command
	}
	if files := s.fab.LogFiles(st.Service); len(files) > 0 {
		out.LogPath = files[0]
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil {
				out.LogBytes += fi.Size()
			}
		}
	}
	return out
}
