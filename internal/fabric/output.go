package fabric

import (
	"sync"
)

// subscriberChunks is the channel depth for one attached reader. See the note in Attach: the byte
// budget is the limit that is meant to matter, so this is generous enough not to bind first when a
// PTY produces many small reads.
const subscriberChunks = 4096

// DefaultOutputBytes is how much recent output the fabric keeps per process.
//
// This is raw bytes, not lines and not a parsed grid: Phase 1 has no terminal emulator (see
// DESIGN.md), so what we keep is exactly what the child wrote, escape sequences and all. Replaying
// it into a terminal reproduces the screen because the *terminal* is the emulator.
const DefaultOutputBytes = 256 << 10 // 256 KiB

// OutputBuffer holds the most recent output of one process and fans it out to attached readers.
//
// Two jobs, and the first is the one that must never fail: something has to read the PTY
// continuously. A pty has a small kernel buffer, and a process whose output is not drained blocks
// in write() - so a server running unattended with nobody attached would freeze, which is the
// opposite of this project's entire promise. Writes here therefore always succeed; when the ring
// is full the oldest bytes are dropped.
//
// The second job is giving a newly attached client something to look at, and telling it honestly
// when it has missed data.
type OutputBuffer struct {
	mu sync.Mutex

	buf   []byte // ring storage, len(buf) == capacity
	start int    // index of the oldest byte
	size  int    // bytes currently held
	// written is the total number of bytes ever written, which is also the stream offset just
	// past the newest byte. Subscribers use it to tell whether they fell behind.
	written int64

	subs   map[int]*Subscriber
	nextID int
	closed bool

	// sink is the disk writer draining this buffer, when there is one, and sinkErr is why
	// there is not. They live here rather than on the Supervisor because the sink is one per
	// buffer: a supervisor is replaced on every stop/start (see Fabric.replaceSupervisor) and
	// hands the buffer to its successor, and a second sink on the same file would write every
	// byte twice.
	sink    *LogSink
	sinkErr string
}

// NewOutputBuffer makes a buffer holding at most capacity bytes.
func NewOutputBuffer(capacity int) *OutputBuffer {
	if capacity <= 0 {
		capacity = DefaultOutputBytes
	}
	return &OutputBuffer{
		buf:  make([]byte, capacity),
		subs: make(map[int]*Subscriber),
	}
}

// Write records output. It never returns an error and never blocks on a slow reader: dropping the
// oldest scrollback is recoverable, wedging the child process is not.
func (o *OutputBuffer) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// The closed check comes first, before the empty-write short-circuit. The other order let
	// Write(nil) return success on a closed buffer while Write(oneByte) returned an error, so
	// whether the caller learned about its stale handle depended on how much it happened to be
	// writing. The comment below says a write after close is a bug worth surfacing; this makes
	// that true for every write.
	if o.closed {
		// Report it rather than pretending. A write after close means the caller has a stale
		// handle, which is a bug worth surfacing.
		return 0, ErrOutputClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	o.append(p)
	o.written += int64(len(p))

	for _, s := range o.subs {
		s.offer(p, o.written)
	}
	o.pruneLaggedLocked()
	return len(p), nil
}

// pruneLaggedLocked drops subscribers that have fallen too far behind.
//
// offer cannot do it: it runs inside the range loop above, with o.mu already held, and deleting
// from a map being ranged over is asking for trouble. So a lagged subscriber used to have its
// channel closed - waking its reader, which is the important part - and then stay in the map
// forever, iterated on every single write. A reader that re-subscribes after lagging (the disk log
// writer does exactly that) left one dead entry behind each time.
func (o *OutputBuffer) pruneLaggedLocked() {
	for id, s := range o.subs {
		if s.Lagged() {
			delete(o.subs, id)
		}
	}
}

// append copies p into the ring, dropping the oldest bytes if it does not fit.
func (o *OutputBuffer) append(p []byte) {
	capacity := len(o.buf)

	// More than the whole ring: keep only the tail. Everything older is gone regardless.
	if len(p) >= capacity {
		copy(o.buf, p[len(p)-capacity:])
		o.start = 0
		o.size = capacity
		return
	}

	// Write position is start+size, wrapped.
	end := (o.start + o.size) % capacity
	n := copy(o.buf[end:], p)
	if n < len(p) {
		copy(o.buf, p[n:])
	}

	if o.size+len(p) <= capacity {
		o.size += len(p)
		return
	}
	// Overwrote the oldest bytes: the window slides forward.
	overflow := o.size + len(p) - capacity
	o.start = (o.start + overflow) % capacity
	o.size = capacity
}

// Snapshot returns a copy of the buffered output, oldest byte first, and the stream offset just
// past its last byte.
func (o *OutputBuffer) Snapshot() ([]byte, int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.snapshotLocked()
}

func (o *OutputBuffer) snapshotLocked() ([]byte, int64) {
	out := make([]byte, o.size)
	if o.size == 0 {
		return out, o.written
	}
	capacity := len(o.buf)
	n := copy(out, o.buf[o.start:min(o.start+o.size, capacity)])
	if n < o.size {
		copy(out[n:], o.buf[:o.size-n])
	}
	return out, o.written
}

// Len returns how many bytes are currently buffered.
func (o *OutputBuffer) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.size
}

// Written returns the total number of bytes ever written.
func (o *OutputBuffer) Written() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.written
}

// Attach returns the current contents and a Subscriber receiving everything written afterwards,
// with no gap between the two: both are taken under one lock, so a client cannot miss bytes that
// land between its snapshot and its subscription. (Doing these as two calls is the obvious
// implementation and has a race that shows up as one corrupted screen in a thousand attaches.)
//
// queueBytes bounds how far a slow reader may fall behind before it is marked lagged.
func (o *OutputBuffer) Attach(queueBytes int) ([]byte, *Subscriber, error) {
	if queueBytes <= 0 {
		queueBytes = DefaultOutputBytes
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.closed {
		return nil, nil, ErrOutputClosed
	}

	snap, at := o.snapshotLocked()

	s := &Subscriber{
		// Two things bound a subscriber: how many bytes it may fall behind (queueBytes, the
		// meaningful one) and how many chunks fit in the channel. The channel is sized so it is
		// not the binding constraint for the realistic case of many small PTY reads - otherwise
		// a client doing fine on bytes gets marked lagged purely because the writes were small,
		// which is a confusing lie.
		ch:       make(chan []byte, subscriberChunks),
		maxBytes: queueBytes,
		owner:    o,
		id:       o.nextID,
		from:     at,
	}
	o.subs[s.id] = s
	o.nextID++
	return snap, s, nil
}

// Close stops the buffer and closes every subscriber's channel.
func (o *OutputBuffer) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	for id, s := range o.subs {
		s.closeCh()
		delete(o.subs, id)
	}
}

// SetSink records the disk writer for this buffer. It is set once, when the buffer is created.
func (o *OutputBuffer) SetSink(s *LogSink) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sink = s
	o.sinkErr = ""
}

// SetSinkError records that this buffer has no disk writer, and why.
func (o *OutputBuffer) SetSinkError(msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sinkErr = msg
}

// LogError reports what is wrong with this buffer's log, or "" when nothing is - including the
// ordinary case of a service whose logging is off, which is a choice rather than a fault.
func (o *OutputBuffer) LogError() string {
	o.mu.Lock()
	sink, msg := o.sink, o.sinkErr
	o.mu.Unlock()
	if msg != "" {
		return msg
	}
	if sink == nil {
		return ""
	}
	return sink.Err()
}

// Sink is the disk writer for this buffer, or nil when there is none.
func (o *OutputBuffer) Sink() *LogSink {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sink
}

// Subscribers reports how many readers are attached. For tests, and for noticing when one is
// never cleaned up.
func (o *OutputBuffer) Subscribers() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.subs)
}

// LogPath is the file this buffer is written to, or "" when it is not written anywhere.
func (o *OutputBuffer) LogPath() string {
	o.mu.Lock()
	sink := o.sink
	o.mu.Unlock()
	if sink == nil {
		return ""
	}
	return sink.Path()
}

// Subscriber delivers live output to one attached reader.
type Subscriber struct {
	ch       chan []byte
	maxBytes int
	owner    *OutputBuffer
	id       int
	from     int64

	// closeOnce guards ch, which three different paths want to close: Detach, the buffer's
	// Close, and the lag path below.
	closeOnce sync.Once

	mu     sync.Mutex
	queued int
	lagged bool
}

// closeCh ends the subscription exactly once, whichever path gets there first.
func (s *Subscriber) closeCh() { s.closeOnce.Do(func() { close(s.ch) }) }

// C is the channel of output chunks. It is closed when the buffer closes, when the subscriber is
// detached, or when the subscriber falls too far behind - in that last case Lagged() reports why,
// and the reader should re-Attach. Chunks are owned by the receiver and are not reused.
func (s *Subscriber) C() <-chan []byte { return s.ch }

// From is the stream offset this subscriber started at.
func (s *Subscriber) From() int64 { return s.from }

// Lagged reports whether this subscriber has missed output because it could not keep up.
//
// It exists because the alternative - dropping bytes quietly - would hand a client a view that is
// wrong in a way it cannot detect. A lagged client should re-Attach to resynchronise. Design rule
// 1: never silently succeed at doing nothing.
func (s *Subscriber) Lagged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lagged
}

// offer queues a chunk. Called with the owner's lock held, so it must not block.
func (s *Subscriber) offer(p []byte, _ int64) {
	s.mu.Lock()
	if s.lagged {
		s.mu.Unlock()
		return
	}
	if s.queued+len(p) > s.maxBytes {
		s.lagged = true
		s.mu.Unlock()
		// Wake the reader by ending the stream. Without this, a subscriber that lagged while
		// its queue happened to be empty was left blocked on a channel that would never
		// receive anything again: the fact that it had missed data was recorded on Lagged(),
		// which a reader sitting in `range sub.C()` is by definition not consulting. A report
		// filed where nobody is looking is design rule 1 all over again.
		s.closeCh()
		return
	}
	s.queued += len(p)
	s.mu.Unlock()

	chunk := make([]byte, len(p))
	copy(chunk, p)

	select {
	case s.ch <- chunk:
	default:
		// Channel full even though the byte budget allowed it: treat as lagging rather than
		// blocking the writer, which would block the PTY drain, which would wedge the child.
		s.mu.Lock()
		s.lagged = true
		s.queued -= len(p)
		s.mu.Unlock()
		s.closeCh()
	}
}

// Consumed tells the subscriber that n bytes have been processed, freeing queue budget.
func (s *Subscriber) Consumed(n int) {
	s.mu.Lock()
	s.queued -= n
	if s.queued < 0 {
		s.queued = 0
	}
	s.mu.Unlock()
}

// Detach removes this subscriber and closes its channel.
func (s *Subscriber) Detach() {
	o := s.owner
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.subs[s.id]; !ok {
		return
	}
	delete(o.subs, s.id)
	s.closeCh()
}
