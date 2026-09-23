package fabric

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultLogBytes is how large one service's log file may grow before it is rotated.
//
// A shell you live in produces output forever, so "keep everything" is not an option on a VPS with
// a small disk. Two generations of this size is the ceiling per service.
const DefaultLogBytes = 16 << 20 // 16 MiB

// logSinkQueueBytes is how far the disk writer may fall behind before the buffer declares it
// lagged. Generous: this reader writes to a local file and should never lag, and when it does the
// honest thing is to say how much was lost - which it does - rather than to wedge the pty drain.
const logSinkQueueBytes = 8 << 20 // 8 MiB

// LogPath is where a service's output is kept.
func LogPath(dir, name string) string { return filepath.Join(dir, name+".log") }

// LogSink appends one service's output to a file.
//
// It is a *subscriber*, not a hook inside OutputBuffer.Write. That is the whole design: Write runs
// on the goroutine draining the pty, under the buffer's lock, and a write() to a full disk or an
// NFS mount that has gone away would block it - which blocks the drain, which wedges the child.
// See the note at the top of OutputBuffer. Falling behind here costs scrollback in a file and says
// so; blocking there costs the process.
//
// Nothing is buffered in userspace. There is no flush to forget on a crash, and O_APPEND means the
// successor of a daemon that exec'd itself can open the same file and carry on without a truncate
// race.
type LogSink struct {
	out  *OutputBuffer
	path string
	max  int64

	// Written only by run.
	f *os.File
	n int64
	// marked is when the time index was last written; see mark.
	marked time.Time
	// noRotateBefore holds off the next attempt after a rotation failed, until another max
	// bytes have been written - rather than retrying the rename on every write.
	noRotateBefore int64
	flushed        int64 // stream offset up to which output has been handed to the file

	mu      sync.Mutex
	sub     *Subscriber
	closing bool
	lastErr string
	// rotateErr is kept separately from lastErr because it is sticky.
	//
	// A write error is transient - a full disk that gets space back should stop being reported -
	// so a successful write clears lastErr. A *rotation* failure is not transient: the file goes
	// on growing past its cap forever afterwards, and the very next successful write was wiping
	// the only record that anything was wrong. `gozellij status` then said nothing at all while
	// the log ate the disk.
	rotateErr string
	dropped   int64

	done      chan struct{}
	closeOnce sync.Once
}

// NewLogSink opens path and starts draining out into it. maxBytes <= 0 means DefaultLogBytes.
func NewLogSink(out *OutputBuffer, path string, maxBytes int64) (*LogSink, error) {
	return openLogSink(out, path, maxBytes, nil, "log opened")
}

// openLogSink is NewLogSink, optionally carrying on from where another writer on the same buffer
// stopped: resume is the stream offset that writer had reached, and only what came after it is
// written. why is what the header line says happened.
func openLogSink(out *OutputBuffer, path string, maxBytes int64, resume *int64, why string) (*LogSink, error) {
	if out == nil {
		return nil, fmt.Errorf("log sink for %s: no output buffer", path)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultLogBytes
	}
	// 0700: the log contains everything the service printed, which for a shell is everything you
	// typed at it and everything it answered. That is nobody else's business.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the log directory for %s: %w", path, err)
	}
	f, err := openLog(path)
	if err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("finding the end of %s: %w", path, err)
	}

	snap, sub, err := out.Attach(logSinkQueueBytes)
	if err != nil {
		f.Close()
		return nil, err
	}

	l := &LogSink{
		out:  out,
		path: path,
		max:  maxBytes,
		f:    f,
		n:    size,
		sub:  sub,
		done: make(chan struct{}),
	}
	// Say where one run of the daemon stops and the next begins.
	//
	// Without this, a log that survives a daemon being replaced reads as one continuous stream,
	// so a service that printed the same line in two different runs looks like it printed it
	// twice in one - and "what did that build print" gets the wrong answer confidently. The
	// supervisor already writes a line when it restarts a process (see waitBackoff); this is the
	// same courtesy for the boundary the supervisor cannot see.
	l.note(fmt.Sprintf("\r\n[gozellij] --- %s: %s by daemon pid %d at %s ---\r\n",
		filepath.Base(path), why, os.Getpid(), time.Now().Format(time.RFC3339)))

	// Whatever was already in the ring when we opened predates us. Write it: on a fresh daemon
	// the ring is empty and this is a no-op, and on a sink opened for a service that was already
	// running it is the difference between the log starting now and the log starting when the
	// service did.
	if resume != nil {
		// Carrying on from a writer that has just stopped. Most of the ring is already in the
		// file it wrote, and catchUp writes only the part that is not - or says how much was
		// lost, if the ring wrapped in between.
		l.flushed = *resume
		l.catchUp(snap, sub.From())
	} else {
		l.emit(snap)
		l.flushed = sub.From()
	}

	// Register with the buffer rather than making the caller do it. The buffer is what outlives
	// supervisors, so it is where "who is writing this to disk, and what is wrong with it" has
	// to be answerable from.
	out.SetSink(l)

	go l.run()
	return l, nil
}

// moveTo renames this writer's files and carries on writing under the new name, for a service
// renamed while it runs. The writer is stopped first - a writer still open on the old path would
// recreate it at its next rotation - and a new one picks up at exactly the offset it reached, so
// nothing is written twice and anything that could not be kept is said.
//
// Files that cannot be moved are left where they are and reported; the new writer still goes
// where the service's name says, because that is where the next reader will look.
func (l *LogSink) moveTo(to string) (*LogSink, error) {
	if !l.Close() {
		return nil, fmt.Errorf("the log writer for %s did not stop in time; the log was left where it is", l.path)
	}
	var errs []error
	for _, suffix := range []string{"", ".1", IndexSuffix, ".1" + IndexSuffix} {
		from := l.path + suffix
		if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := os.Stat(to + suffix); err == nil {
			errs = append(errs, fmt.Errorf("%s already exists; %s was left where it is", to+suffix, from))
			continue
		}
		if err := os.Rename(from, to+suffix); err != nil {
			errs = append(errs, err)
		}
	}
	flushed := l.flushed // the writer has finished, so this is final
	next, err := openLogSink(l.out, to, l.max, &flushed, "renamed from "+filepath.Base(l.path))
	if err != nil {
		errs = append(errs, err)
	}
	return next, errors.Join(errs...)
}

// The time index.
//
// A log is the raw bytes a service wrote to its terminal, escape sequences and all, and a timestamp
// written into it would land in the middle of whatever a full-screen program was drawing. So the
// times go beside it instead: <name>.log.idx holds lines of "unix-seconds byte-offset", one at most
// every MarkEvery, each saying where in the log the output from that moment on begins. That is what
// `gozellij logs -since` reads - accurate to the interval, and erring towards showing a little more,
// never less.

// IndexSuffix is appended to a log's path to name its index.
const IndexSuffix = ".idx"

// IndexPath is the index of the log at path.
func IndexPath(path string) string { return path + IndexSuffix }

// MarkEvery is how often, at most, the index gets a line. A variable so tests need not wait.
var MarkEvery = time.Minute

// mark records where output from now on begins, if the last mark is old enough. Failing to write
// it is not worth failing the log over: the log is the record, the index a way into it.
func (l *LogSink) mark() {
	now := time.Now()
	if !l.marked.IsZero() && now.Sub(l.marked) < MarkEvery {
		return
	}
	f, err := os.OpenFile(IndexPath(l.path), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%d %d\n", now.Unix(), l.n)
	_ = f.Close()
	l.marked = now
}

func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return f, nil
}

// Path is the file being written.
func (l *LogSink) Path() string { return l.path }

// Err reports what is wrong with this log, or "" when nothing is.
//
// A log that stopped being written is exactly the kind of failure this project refuses to keep
// quiet about: the operator finds out when they go looking for output that was never there, which
// is the worst possible moment. Supervisor.Status carries this out to `gozellij status`.
func (l *LogSink) Err() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var parts []string
	if l.lastErr != "" {
		parts = append(parts, fmt.Sprintf("%s (%d bytes of output not written)", l.lastErr, l.dropped))
	}
	if l.rotateErr != "" {
		parts = append(parts, fmt.Sprintf("cannot rotate this log, so it will grow past its %d byte limit: %s",
			l.max, l.rotateErr))
	}
	return strings.Join(parts, "; ")
}

// CloseWait is how long Close waits for the writer to put the file down.
//
// Bounded, because the writer can be stuck in write(2) on a mount that has gone away - the exact
// situation this whole design keeps off the pty drain - and `gozellij rm` waits for this while
// holding the service's lifecycle lock. Waiting for ever there would turn a hung disk into a hung
// command holding a lock nothing else can take.
const CloseWait = 5 * time.Second

// Close stops writing and waits for the file to be closed.
//
// It reports whether the writer actually finished. A caller that is about to delete the file needs
// to know: if the writer is still going, anything left in its queue can be written after the
// deletion, and rotation's O_CREATE would put the file back.
func (l *LogSink) Close() bool {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closing = true
		sub := l.sub
		l.mu.Unlock()
		sub.Detach()
	})
	select {
	case <-l.done:
		return true
	case <-time.After(CloseWait):
		return false
	}
}

// run drains the subscription into the file until the buffer closes or Close is called.
func (l *LogSink) run() {
	defer close(l.done)
	defer func() {
		if l.f != nil {
			l.f.Close()
			l.f = nil
		}
	}()

	for {
		l.mu.Lock()
		sub := l.sub
		l.mu.Unlock()

		for chunk := range sub.C() {
			l.emit(chunk)
			l.flushed += int64(len(chunk))
			sub.Consumed(len(chunk))
		}

		// The channel closed. Three reasons, and they need different answers: we were closed,
		// the buffer was closed, or we fell behind and the buffer woke us by ending the
		// stream. Only the last one is recoverable.
		l.mu.Lock()
		closing := l.closing
		l.mu.Unlock()
		if closing || !sub.Lagged() {
			return
		}

		snap, next, err := l.out.Attach(logSinkQueueBytes)
		if err != nil {
			// The buffer is gone, so there is nothing left to follow.
			return
		}
		l.mu.Lock()
		l.sub = next
		closing = l.closing
		l.mu.Unlock()
		if closing {
			next.Detach()
			continue
		}
		l.catchUp(snap, next.From())
	}
}

// catchUp writes the part of a fresh snapshot the file has not already got.
//
// Re-attaching after lagging hands back the whole ring, most of which is usually already on disk.
// Writing it wholesale would duplicate it - a log that repeats itself at every hiccup is worse
// than one with a gap, because a gap is visible and a duplicate is not. The stream offsets say
// exactly where we are: `at` is the offset just past the snapshot, so the snapshot covers
// [at-len(snap), at).
func (l *LogSink) catchUp(snap []byte, at int64) {
	start := at - int64(len(snap))

	switch {
	case l.flushed >= at:
		// Nothing in this snapshot is new.
	case l.flushed >= start:
		l.emit(snap[l.flushed-start:])
	default:
		// The ring wrapped past where we were. Those bytes exist nowhere now, so say how many
		// rather than leaving a seam nobody can see. Design rule 1.
		missed := start - l.flushed
		l.note(fmt.Sprintf("\r\n[gozellij] %d bytes of output never reached this log: "+
			"the log writer fell behind and the buffer wrapped\r\n", missed))
		l.mu.Lock()
		l.dropped += missed
		l.mu.Unlock()
		l.emit(snap)
	}
	l.flushed = at
}

// note writes a line of gozellij's own, which is not part of the stream and so does not advance
// the flushed offset.
func (l *LogSink) note(s string) { l.emit([]byte(s)) }

// emit writes to the file, rotating first if this would take it over the limit.
//
// A failed write is recorded and counted, and the drain carries on: a full disk must not stop us
// reading the pty, and a disk that has space again should start working without a restart.
func (l *LogSink) emit(p []byte) {
	if len(p) == 0 {
		return
	}
	if l.f == nil {
		f, err := openLog(l.path)
		if err != nil {
			l.fail(err, len(p))
			return
		}
		size, serr := f.Seek(0, io.SeekEnd)
		if serr != nil {
			f.Close()
			l.fail(serr, len(p))
			return
		}
		l.f, l.n = f, size
	}

	if l.max > 0 && l.n+int64(len(p)) > l.max && l.n >= l.noRotateBefore {
		l.rotate()
		if l.f == nil {
			l.fail(fmt.Errorf("rotating %s left no file open", l.path), len(p))
			return
		}
	}
	l.mark()

	n, err := l.f.Write(p)
	l.n += int64(n)
	if err != nil {
		l.fail(err, len(p)-n)
		return
	}
	l.clearErr()
}

// rotate moves the current file aside and starts a new one. One generation is kept: two files of
// max bytes is the ceiling per service, which is a number an operator can reason about.
func (l *LogSink) rotate() {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	rotated := true
	if err := os.Rename(l.path, l.path+".1"); err != nil && !os.IsNotExist(err) {
		// Say so, but carry on: a log that cannot be rotated should keep being written, not
		// stop. The size limit is then not honoured, which is the lesser problem - and is
		// recorded where the next successful write cannot erase it.
		l.setRotateErr(err.Error())
		rotated = false
	} else {
		l.setRotateErr("")
		// The index goes with the file it indexes - only once that file has actually moved.
		// Moved first, a failed rotation left an index for a .1 that never existed.
		_ = os.Rename(IndexPath(l.path), IndexPath(l.path+".1"))
		l.marked = time.Time{}
	}
	f, err := openLog(l.path)
	if err != nil {
		l.fail(err, 0)
		return
	}
	// Where the file really ends, not 0: after a failed rotation this is the same file, still
	// full, and a zero here put every later index mark short by its whole size.
	size, serr := f.Seek(0, io.SeekEnd)
	if serr != nil || rotated {
		size = 0
	}
	l.f, l.n = f, size
	l.noRotateBefore = 0
	if !rotated {
		l.noRotateBefore = size + l.max
	}
}

func (l *LogSink) fail(err error, lost int) {
	l.mu.Lock()
	l.lastErr = err.Error()
	if lost > 0 {
		l.dropped += int64(lost)
	}
	l.mu.Unlock()
}

func (l *LogSink) clearErr() {
	l.mu.Lock()
	l.lastErr = ""
	l.mu.Unlock()
}

func (l *LogSink) setRotateErr(msg string) {
	l.mu.Lock()
	l.rotateErr = msg
	l.mu.Unlock()
}

// ReadLogTail returns the last maxBytes of a service's log, oldest byte first, and whether older
// output was left out.
//
// It reads the rotated generation too, so asking for more than the current file holds does not
// silently answer with less. Returns os.ErrNotExist when the service has no log on disk - the
// caller then falls back to the in-memory ring, which is all there is for a service whose logging
// is off.
func ReadLogTail(dir, name string, maxBytes int) ([]byte, bool, error) {
	if dir == "" {
		return nil, false, os.ErrNotExist
	}
	path := LogPath(dir, name)
	cur, curSize, err := tailFile(path, maxBytes)
	if err != nil {
		return nil, false, err
	}

	total := curSize
	// Still room? The rotated generation holds what came before.
	if maxBytes <= 0 || len(cur) < maxBytes {
		room := 0
		if maxBytes > 0 {
			room = maxBytes - len(cur)
		}
		prev, prevSize, perr := tailFile(path+".1", room)
		switch {
		case perr == nil:
			total += prevSize
			cur = append(prev, cur...)
		case os.IsNotExist(perr):
		default:
			return nil, false, perr
		}
	} else if st, serr := os.Stat(path + ".1"); serr == nil {
		total += st.Size()
	}

	return cur, int64(len(cur)) < total, nil
}

// tailFile returns the last maxBytes of a file (all of it when maxBytes <= 0) and its full size.
func tailFile(path string, maxBytes int) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("sizing %s: %w", path, err)
	}
	size := st.Size()

	want := size
	if maxBytes > 0 && int64(maxBytes) < want {
		want = int64(maxBytes)
	}
	if want <= 0 {
		return nil, size, nil
	}
	if _, err := f.Seek(size-want, io.SeekStart); err != nil {
		return nil, size, fmt.Errorf("seeking in %s: %w", path, err)
	}
	buf := make([]byte, want)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, size, fmt.Errorf("reading %s: %w", path, err)
	}
	return buf[:n], size, nil
}

// ErrNoIndex means a log has no time index to answer -since from: it was written before gozellij
// kept one, or the index could not be written. Not the same as "nothing since then", and never to
// be answered as though it were.
var ErrNoIndex = errors.New("this log has no time index")

type indexMark struct {
	at     int64 // unix seconds
	offset int64
}

func readIndex(path string) []indexMark {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []indexMark
	for _, line := range strings.Split(string(b), "\n") {
		var m indexMark
		if _, err := fmt.Sscanf(line, "%d %d", &m.at, &m.offset); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// sinceStart is where, in one file, the output from since onwards begins, going by its marks.
//
// A mark is written at a write, and only once MarkEvery has passed since the last one - so every
// byte after mark i and before mark i+1 was written within MarkEvery of mark i. When since is past
// that window, nothing in the segment is "since", and the answer starts at the next mark, or at the
// end of the file. Starting at mark i regardless handed back a service's whole last burst, hours
// old, as output from the last ten minutes.
//
// found is false when since is older than every mark, and then the file is wanted from its start.
func sinceStart(marks []indexMark, since int64, size int64) (off int64, found bool) {
	last := -1
	for i, m := range marks {
		if m.at > since {
			break
		}
		last = i
	}
	if last < 0 {
		return 0, false
	}
	if since < marks[last].at+int64(MarkEvery/time.Second) || MarkEvery < time.Second {
		off = marks[last].offset
	} else if last+1 < len(marks) {
		off = marks[last+1].offset
	} else {
		off = size
	}
	if off > size {
		off = size
	}
	return off, true
}

// LogSince is what ReadLogSince found.
type LogSince struct {
	Data      []byte
	Truncated bool
	// Unknown is how many of the bytes in Data come from before the time index begins, so
	// nothing says when they were written. They are included when since reaches back past the
	// oldest mark, because leaving them out would hide output that may well be recent - and
	// counted, so the answer can say so rather than present them as filtered.
	Unknown int64
}

// ReadLogSince is what a service wrote from since onwards, at most maxBytes of the newest of it.
//
// Accurate to MarkEvery, erring towards a little more. When since is older than the oldest mark,
// everything kept is the answer, with Unknown saying how much of it has no time; when there is no
// index at all, ErrNoIndex.
func ReadLogSince(dir, name string, since time.Time, maxBytes int) (LogSince, error) {
	if dir == "" {
		return LogSince{}, os.ErrNotExist
	}
	path := LogPath(dir, name)
	// A rotation between reading the file and reading its index would pair one with the other's
	// successor, and the old file could come back twice. Read, then check the file is still the
	// one that was read; if it is not, read again.
	for attempt := 0; ; attempt++ {
		before, err := os.Stat(path)
		if err != nil {
			return LogSince{}, err
		}
		got, err := readLogSinceOnce(path, since, maxBytes)
		after, serr := os.Stat(path)
		if err != nil || (serr == nil && os.SameFile(before, after)) || attempt == 3 {
			return got, err
		}
	}
}

func readLogSinceOnce(path string, since time.Time, maxBytes int) (LogSince, error) {
	cur, err := os.ReadFile(path)
	if err != nil {
		return LogSince{}, err
	}
	prevMarks, curMarks := readIndex(IndexPath(path+".1")), readIndex(IndexPath(path))
	if len(prevMarks) == 0 && len(curMarks) == 0 {
		return LogSince{}, ErrNoIndex
	}

	var out LogSince
	if off, ok := sinceStart(curMarks, since.Unix(), int64(len(cur))); ok {
		// Everything asked for is in the current file.
		out.Data = cur[off:]
	} else {
		// It begins in the rotated generation, or before anything that was kept.
		prev, perr := os.ReadFile(path + ".1")
		if perr != nil && !os.IsNotExist(perr) {
			return LogSince{}, perr
		}
		if off, ok := sinceStart(prevMarks, since.Unix(), int64(len(prev))); ok {
			prev = prev[off:]
		} else {
			// From the start of what was kept. Whatever precedes the oldest mark has no time.
			switch {
			case len(prevMarks) > 0:
				out.Unknown = prevMarks[0].offset
			case len(prev) > 0:
				out.Unknown = int64(len(prev))
			default:
				out.Unknown = curMarks[0].offset
			}
		}
		out.Data = append(prev, cur...)
	}

	if maxBytes > 0 && len(out.Data) > maxBytes {
		cut := int64(len(out.Data) - maxBytes)
		out.Data, out.Truncated = out.Data[cut:], true
		if out.Unknown -= cut; out.Unknown < 0 {
			out.Unknown = 0
		}
	}
	return out, nil
}
