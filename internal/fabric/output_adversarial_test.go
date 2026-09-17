package fabric

// Adversarial tests for OutputBuffer, written independently of output_test.go.
//
// Every test here is deterministic: where goroutines are involved the assertion is an invariant
// that must hold at *any* interleaving, never a claim about which goroutine got there first.

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// advStream makes n bytes of content in which no two windows of any useful length are equal, so
// that a duplicated or dropped chunk cannot be masked by identical neighbours. Plain 'x' repeated
// would let "lost one, duplicated another" pass a length check - which is exactly what the
// existing no-gap test checks.
func advStream(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		// Two interleaved primes give a period far longer than any ring used here.
		b[i] = byte((i*7 + (i/251)*13) % 256)
	}
	return b
}

// advTail is what a ring of the given capacity must hold after the whole stream was written.
func advTail(stream []byte, capacity int) []byte {
	if len(stream) <= capacity {
		return stream
	}
	return stream[len(stream)-capacity:]
}

// advCheckRing compares the buffer against a reference model after each step: contents must be
// the last min(total, cap) bytes, the offset must be the total, and Len must agree.
func advCheckRing(t *testing.T, step string, o *OutputBuffer, capacity int, all []byte) {
	t.Helper()
	got, at := o.Snapshot()
	want := advTail(all, capacity)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: Snapshot = %q, want %q", step, got, want)
	}
	if at != int64(len(all)) {
		t.Fatalf("%s: offset = %d, want %d", step, at, len(all))
	}
	if o.Len() != len(want) {
		t.Fatalf("%s: Len = %d, want %d", step, o.Len(), len(want))
	}
	if o.Written() != int64(len(all)) {
		t.Fatalf("%s: Written = %d, want %d", step, o.Written(), len(all))
	}
}

// Every boundary the ring arithmetic has: a write of cap-1, then the one byte that fills it
// exactly, then the one byte that first evicts; a write of exactly cap onto a partly full ring; a
// write of cap+1; and repeated writes that land precisely on the wrap point again and again.
func TestAdvRingArithmeticAtExactBoundaries(t *testing.T) {
	for _, capacity := range []int{1, 2, 3, 8, 64} {
		t.Run(fmt.Sprintf("cap=%d", capacity), func(t *testing.T) {
			o := NewOutputBuffer(capacity)
			src := advStream(64 * capacity)
			var all []byte
			next := 0
			write := func(step string, n int) {
				t.Helper()
				p := src[next : next+n]
				next += n
				k, err := o.Write(p)
				if err != nil || k != n {
					t.Fatalf("%s: Write(%d bytes) = %d, %v", step, n, k, err)
				}
				all = append(all, p...)
				advCheckRing(t, step, o, capacity, all)
			}

			if capacity > 1 {
				write("cap-1", capacity-1)
				write("the byte that fills it exactly", 1)
				write("the byte that first evicts", 1)
			}
			write("exactly cap onto a partly full ring", capacity)
			write("cap+1", capacity+1)
			write("one more byte after a >cap write", 1)

			// Repeated writes that each land exactly on the wrap: after this the write cursor is
			// at zero every time, which is the case a wrong modulo hides.
			for i := 0; i < 5; i++ {
				o2 := NewOutputBuffer(capacity)
				var all2 []byte
				for j := 0; j < 3+i; j++ {
					p := src[(j*capacity)%len(src) : (j*capacity)%len(src)+capacity]
					if _, err := o2.Write(p); err != nil {
						t.Fatal(err)
					}
					all2 = append(all2, p...)
					advCheckRing(t, fmt.Sprintf("full-ring write #%d", j), o2, capacity, all2)
				}
				// ...then a small write straddling nothing, then one straddling the wrap.
				if capacity > 2 {
					half := capacity / 2
					p := src[:half]
					if _, err := o2.Write(p); err != nil {
						t.Fatal(err)
					}
					all2 = append(all2, p...)
					advCheckRing(t, "half write after full-ring writes", o2, capacity, all2)
					p = src[half : half+capacity-1]
					if _, err := o2.Write(p); err != nil {
						t.Fatal(err)
					}
					all2 = append(all2, p...)
					advCheckRing(t, "write straddling the wrap", o2, capacity, all2)
				}
			}
		})
	}
}

// A random-ish walk over write sizes against the same reference model. The sizes are derived
// from a fixed sequence, so the walk is reproducible; it is here to cover the interactions the
// hand-picked boundaries above do not.
func TestAdvRingMatchesReferenceModelOverManyWrites(t *testing.T) {
	const capacity = 37 // prime, so nothing lines up by accident
	o := NewOutputBuffer(capacity)
	src := advStream(20000)
	var all []byte
	next := 0
	for i := 0; next < len(src)-2*capacity; i++ {
		// sizes cycle through 0..2*cap+1 in a scrambled order
		n := (i*11 + 3) % (2*capacity + 2)
		p := src[next : next+n]
		next += n
		if _, err := o.Write(p); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		all = append(all, p...)
		advCheckRing(t, fmt.Sprintf("write %d (%d bytes)", i, n), o, capacity, all)
	}
}

// Snapshot must never return a slice that shares storage with the ring: a later Write must not
// change what an earlier Snapshot handed out.
func TestAdvSnapshotIsACopyNotAView(t *testing.T) {
	o := NewOutputBuffer(8)
	mustWrite(t, o, "abcdefgh")
	snap, _ := o.Snapshot()
	mustWrite(t, o, "ZZZZZZZZ")
	if string(snap) != "abcdefgh" {
		t.Fatalf("earlier snapshot was mutated by a later write: %q", snap)
	}
	// The same for the snapshot handed out by Attach, and for live chunks.
	o2 := NewOutputBuffer(8)
	mustWrite(t, o2, "12345678")
	snap2, sub, err := o2.Attach(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Detach()
	src := []byte("live")
	if _, err := o2.Write(src); err != nil {
		t.Fatal(err)
	}
	src[0] = '!' // caller reuses its buffer, as a PTY drain loop does
	chunk := <-sub.C()
	if string(chunk) != "live" {
		t.Fatalf("live chunk aliases the writer's buffer: %q", chunk)
	}
	if string(snap2) != "12345678" {
		t.Fatalf("attach snapshot was mutated: %q", snap2)
	}
}

// Concurrent Snapshot against a live writer. The invariant that must hold at *every* interleaving:
// a snapshot with offset `at` is exactly stream[at-len(snap):at]. That is true whichever goroutine
// wins; a torn read (start/size/written not updated atomically) breaks it.
func TestAdvConcurrentSnapshotIsAlwaysAContiguousWindow(t *testing.T) {
	const capacity = 100
	stream := advStream(50000)
	o := NewOutputBuffer(capacity)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < len(stream); {
			n := 1 + (i % 13)
			if i+n > len(stream) {
				n = len(stream) - i
			}
			if _, err := o.Write(stream[i : i+n]); err != nil {
				return
			}
			i += n
		}
	}()

	var checked int
	for {
		snap, at := o.Snapshot()
		if int64(len(snap)) > at {
			t.Fatalf("snapshot longer (%d) than the total ever written (%d)", len(snap), at)
		}
		if len(snap) > capacity {
			t.Fatalf("snapshot longer (%d) than the ring (%d)", len(snap), capacity)
		}
		want := stream[int(at)-len(snap) : at]
		if !bytes.Equal(snap, want) {
			t.Fatalf("snapshot at offset %d is not the stream window ending there", at)
		}
		checked++
		if at == int64(len(stream)) {
			break
		}
	}
	wg.Wait()
	if checked < 2 {
		t.Logf("only %d snapshot(s) taken; the writer finished before the reader got going", checked)
	}
}

// The sharper version of the no-gap claim: not just "the byte counts add up" but "the snapshot is
// exactly stream[:From()] and the live chunks are exactly stream[From():]", with content that
// cannot be duplicated or dropped without detection. Lag is impossible by arithmetic (chunks <
// subscriberChunks, bytes < budget, ring holds the whole stream), so the outcome does not depend
// on where Attach lands - only the coverage does.
func TestAdvAttachSplitsTheStreamExactlyAtFrom(t *testing.T) {
	const chunks = 2000
	const chunkLen = 3
	stream := advStream(chunks * chunkLen)
	o := NewOutputBuffer(len(stream)) // the ring holds everything, so snap must be the full prefix

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < chunks; i++ {
			if _, err := o.Write(stream[i*chunkLen : (i+1)*chunkLen]); err != nil {
				return
			}
			if i%100 == 0 {
				time.Sleep(50 * time.Microsecond) // coverage only: lets Attach land mid-stream
			}
		}
	}()

	time.Sleep(time.Millisecond)
	snap, sub, err := o.Attach(len(stream) + 1)
	if err != nil {
		t.Fatal(err)
	}
	var live bytes.Buffer
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for chunk := range sub.C() {
			live.Write(chunk)
			sub.Consumed(len(chunk))
		}
	}()
	wg.Wait()
	sub.Detach()
	<-drained

	if sub.Lagged() {
		t.Fatal("lagged although lag was impossible by arithmetic")
	}
	from := sub.From()
	if int64(len(snap)) != from {
		t.Fatalf("snapshot has %d bytes but From() = %d", len(snap), from)
	}
	if !bytes.Equal(snap, stream[:from]) {
		t.Fatal("snapshot is not exactly stream[:From()]")
	}
	if !bytes.Equal(live.Bytes(), stream[from:]) {
		t.Fatalf("live stream (%d bytes) is not exactly stream[From():] (%d bytes)", live.Len(), len(stream)-int(from))
	}
	t.Logf("attach landed at offset %d of %d", from, len(stream))
}

// Same property when the ring is smaller than the stream: the snapshot is then the last
// min(cap, From) bytes before From, and the live part is still exactly stream[From():].
func TestAdvAttachSplitsExactlyWhenTheRingHasEvicted(t *testing.T) {
	const capacity = 64
	stream := advStream(3000)
	o := NewOutputBuffer(capacity)

	// Fully sequential: write half, attach, write the rest.
	half := 1700
	for i := 0; i < half; i += 7 {
		end := min(i+7, half)
		if _, err := o.Write(stream[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	snap, sub, err := o.Attach(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	from := sub.From()
	if from != int64(half) {
		t.Fatalf("From() = %d, want %d", from, half)
	}
	if !bytes.Equal(snap, stream[half-capacity:half]) {
		t.Fatalf("snapshot is not the %d bytes ending at From()", capacity)
	}
	for i := half; i < len(stream); i += 5 {
		end := min(i+5, len(stream))
		if _, err := o.Write(stream[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	sub.Detach()
	var live bytes.Buffer
	for chunk := range sub.C() {
		live.Write(chunk)
	}
	if !bytes.Equal(live.Bytes(), stream[half:]) {
		t.Fatal("live chunks are not exactly stream[From():]")
	}
	if sub.Lagged() {
		t.Fatal("lagged with 260 chunks in a 4096-chunk channel and 1300 bytes in a 1 MiB budget")
	}
}

// The byte budget is inclusive: a queue holding exactly maxBytes is not lagged; one byte more is.
func TestAdvByteBudgetBoundaryIsExact(t *testing.T) {
	o := NewOutputBuffer(1024)
	_, sub, err := o.Attach(10)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Detach()
	mustWrite(t, o, "0123456")
	mustWrite(t, o, "789") // queued = 10 = maxBytes exactly
	if sub.Lagged() {
		t.Fatal("marked lagged with exactly maxBytes queued; the budget should be inclusive")
	}
	if got := advQueued(sub); got != 10 {
		t.Fatalf("queued = %d, want 10", got)
	}
	mustWrite(t, o, "x") // 11 > 10
	if !sub.Lagged() {
		t.Fatal("not marked lagged after exceeding the budget by one byte")
	}
	// The rejected byte must not be counted as queued.
	if got := advQueued(sub); got != 10 {
		t.Fatalf("queued = %d after a rejected write, want 10 (rejected bytes must not count)", got)
	}
	// Everything that *was* accepted must still be delivered, in order: the reader gets a clean
	// prefix and a lag flag, not a truncated prefix.
	var got bytes.Buffer
	for i := 0; i < 2; i++ {
		got.Write(<-sub.C())
	}
	if got.String() != "0123456789" {
		t.Fatalf("delivered %q, want the accepted prefix %q", got.String(), "0123456789")
	}
	// After lagging, the subscription ends: the channel is closed to wake a reader that would
	// otherwise block on a stream that will never produce another byte. So the next receive must
	// yield the zero value with ok=false, and must NOT be a further chunk of data.
	select {
	case c, ok := <-sub.C():
		if ok {
			t.Fatalf("unexpected extra chunk %q after lag", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a lagged subscriber was left blocked; its channel should have been closed")
	}
}

// The channel bound is also exact: subscriberChunks one-byte writes fit; the next one lags. And
// the accounting must be rolled back for the rejected chunk, or a later re-attach-free reader
// would see budget that is not there.
func TestAdvChannelDepthBoundaryIsExactAndAccountingRollsBack(t *testing.T) {
	o := NewOutputBuffer(1 << 20)
	_, sub, err := o.Attach(1 << 20) // byte budget is not the constraint here
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Detach()
	for i := 0; i < subscriberChunks; i++ {
		mustWrite(t, o, "a")
	}
	if sub.Lagged() {
		t.Fatalf("lagged after exactly %d chunks; the channel should hold that many", subscriberChunks)
	}
	if got := advQueued(sub); got != subscriberChunks {
		t.Fatalf("queued = %d, want %d", got, subscriberChunks)
	}
	mustWrite(t, o, "b") // channel full: the byte budget allowed it, the channel did not
	if !sub.Lagged() {
		t.Fatal("not lagged after overfilling the channel")
	}
	if got := advQueued(sub); got != subscriberChunks {
		t.Fatalf("queued = %d after the channel-full rejection, want %d (must roll back)", got, subscriberChunks)
	}
	// Drain: exactly subscriberChunks 'a's, nothing else, and queued goes back to zero.
	n := 0
	for {
		select {
		case c, ok := <-sub.C():
			if !ok {
				// The channel closed because the subscriber lagged. That is the new
				// contract - the reader is woken rather than left hanging - so it ends
				// the drain instead of being read as an empty chunk.
				goto drained
			}
			if string(c) != "a" {
				t.Fatalf("chunk %d = %q, want %q", n, c, "a")
			}
			n++
			sub.Consumed(len(c))
			continue
		default:
		}
		break
	}
drained:
	if n != subscriberChunks {
		t.Fatalf("drained %d chunks, want %d", n, subscriberChunks)
	}
	if got := advQueued(sub); got != 0 {
		t.Fatalf("queued = %d after draining everything, want 0", got)
	}
}

// A reader that keeps up must never be marked lagged, no matter how long it goes on. This walks
// the queue through many fill/drain cycles that each come within one byte of the budget, so any
// drift in `queued` - even one byte per cycle - shows up as a wrongful lag.
func TestAdvWellBehavedReaderNeverDriftsIntoLag(t *testing.T) {
	o := NewOutputBuffer(1 << 16)
	const budget = 100
	_, sub, err := o.Attach(budget)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Detach()

	for cycle := 0; cycle < 500; cycle++ {
		// fill to exactly the budget with odd-sized chunks
		sizes := []int{37, 23, 40} // = 100
		for _, n := range sizes {
			if _, err := o.Write(bytes.Repeat([]byte{'q'}, n)); err != nil {
				t.Fatal(err)
			}
		}
		if sub.Lagged() {
			t.Fatalf("cycle %d: lagged while filled to exactly the budget", cycle)
		}
		for range sizes {
			c := <-sub.C()
			sub.Consumed(len(c))
		}
		if got := advQueued(sub); got != 0 {
			t.Fatalf("cycle %d: queued = %d after a full drain, want 0 (accounting drifted)", cycle, got)
		}
	}
}

// Once lagged, a subscriber stays lagged - and receives nothing further. Un-lagging quietly would
// hand the reader a stream with a hole in it that looks contiguous.
func TestAdvLaggedSubscriberNeverUnlagsAndGetsNoHoleyStream(t *testing.T) {
	o := NewOutputBuffer(1 << 16)
	_, sub, err := o.Attach(4)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Detach()
	mustWrite(t, o, "abcd")
	mustWrite(t, o, "e") // lag
	if !sub.Lagged() {
		t.Fatal("precondition: not lagged")
	}
	c := <-sub.C()
	sub.Consumed(len(c))
	sub.Consumed(1 << 20) // an over-generous Consumed must not buy the lag back either
	if got := advQueued(sub); got != 0 {
		t.Fatalf("queued = %d, want clamped to 0", got)
	}
	mustWrite(t, o, "fghi")
	if !sub.Lagged() {
		t.Fatal("subscriber un-lagged after Consumed; the hole would be invisible")
	}
	select {
	case c, ok := <-sub.C():
		if ok {
			t.Fatalf("lagged subscriber received %q after the hole", c)
		}
		// Closed: correct. The reader is told the stream ended and Lagged() says why.
	default:
		// Also acceptable: nothing pending. What must never happen is more data.
	}
}

// A single write larger than the subscriber's remaining budget with nothing queued marks it lagged -
// and nothing is delivered. A reader blocked in `range sub.C()` is not woken by the lag: it stays
// blocked until Detach or Close, because the lag is reported on Lagged(), which the reader is not
// looking at while it is blocked on the channel.
//
// internal/daemon/attach.go pumpOutput checks Lagged() only after receiving a chunk, so through the
// daemon this shape hangs the attach silently. It is unreachable there today (AttachQueueBytes is
// 4 MiB and a PTY read is at most 32 KiB), but the library API allows any queueBytes.
func TestAdvLagWithEmptyQueueLeavesReaderBlockedForever(t *testing.T) {
	o := NewOutputBuffer(1 << 16)
	_, sub, err := o.Attach(10)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Detach()
	mustWrite(t, o, "elevenbytes") // 11 > 10, nothing queued
	if !sub.Lagged() {
		t.Fatal("precondition: not lagged")
	}
	select {
	case c, ok := <-sub.C():
		if !ok {
			return // channel closed: the reader is woken and can check Lagged(). Fine.
		}
		t.Fatalf("received %q despite lag", c)
	default:
	}
	t.Error("FIXED BUG REGRESSED: a subscriber that lags with an empty queue is not woken - Lagged() is set " +
		"but C() stays open and empty until Detach or Close, so a reader blocked on C() (as daemon " +
		"pumpOutput is) has no way to learn it. Design rule 1: the report is on a channel the reader is " +
		"not reading. Unreachable via the daemon's 4 MiB budget today; reachable via the library API.")
}

// Zero-length writes short-circuit (output.go:58, `len(p) == 0`) before the closed check, so
// Write(nil) on a closed buffer says success while Write of one byte says ErrOutputClosed. io.Writer
// permits it; the buffer's own contract ("a write after close ... is a bug worth surfacing") does
// not quite.
func TestAdvEmptyWriteAfterCloseIsSilentlyOK(t *testing.T) {
	o := NewOutputBuffer(16)
	o.Close()
	n, err := o.Write(nil)
	if n != 0 {
		t.Fatalf("n = %d", n)
	}
	if !errors.Is(err, ErrOutputClosed) {
		t.Error("FIXED BUG REGRESSED: Write(nil) after Close returns (0, nil); the len(p)==0 short-circuit " +
			"runs before the closed check, so a stale handle doing an empty write is told the buffer is fine")
	}
}

// Detach/Close ordering: every order, twice, and with the channel state checked afterwards.
func TestAdvDetachCloseOrderings(t *testing.T) {
	assertClosedEmpty := func(t *testing.T, sub *Subscriber) {
		t.Helper()
		select {
		case c, ok := <-sub.C():
			if ok {
				t.Fatalf("channel still open, delivered %q", c)
			}
		default:
			t.Fatal("channel neither closed nor readable")
		}
	}

	t.Run("detach then close", func(t *testing.T) {
		o := NewOutputBuffer(16)
		_, sub, _ := o.Attach(16)
		sub.Detach()
		o.Close()
		sub.Detach()
		assertClosedEmpty(t, sub)
	})
	t.Run("close then detach", func(t *testing.T) {
		o := NewOutputBuffer(16)
		_, sub, _ := o.Attach(16)
		o.Close()
		sub.Detach() // subscriber already removed by Close: must not double-close
		sub.Detach()
		assertClosedEmpty(t, sub)
	})
	t.Run("detach one of two does not disturb the other", func(t *testing.T) {
		o := NewOutputBuffer(16)
		_, a, _ := o.Attach(16)
		_, b, _ := o.Attach(16)
		a.Detach()
		mustWrite(t, o, "still")
		select {
		case c := <-b.C():
			if string(c) != "still" {
				t.Fatalf("b got %q", c)
			}
		default:
			t.Fatal("b received nothing after a was detached")
		}
		assertClosedEmpty(t, a)
		b.Detach()
	})
	t.Run("write after close reaches nobody", func(t *testing.T) {
		o := NewOutputBuffer(16)
		_, sub, _ := o.Attach(16)
		o.Close()
		if _, err := o.Write([]byte("late")); !errors.Is(err, ErrOutputClosed) {
			t.Fatalf("err = %v", err)
		}
		assertClosedEmpty(t, sub)
		// And the ring is untouched: the closed buffer's snapshot is still readable and unchanged.
		if snap, at := o.Snapshot(); len(snap) != 0 || at != 0 {
			t.Fatalf("closed buffer changed: %q at %d", snap, at)
		}
	})
	t.Run("attach after close returns error and no subscriber", func(t *testing.T) {
		o := NewOutputBuffer(16)
		mustWrite(t, o, "data")
		o.Close()
		snap, sub, err := o.Attach(16)
		if !errors.Is(err, ErrOutputClosed) {
			t.Fatalf("err = %v", err)
		}
		if sub != nil {
			t.Fatal("got a subscriber from a closed buffer")
		}
		if snap != nil {
			t.Fatalf("got a snapshot %q with the error; nil-with-error is the contract", snap)
		}
	})
	t.Run("stale subscriber id after close is not reused", func(t *testing.T) {
		// Detach on a subscriber from a *different* generation must not delete a live one.
		o := NewOutputBuffer(16)
		_, a, _ := o.Attach(16)
		a.Detach()
		_, b, _ := o.Attach(16)
		a.Detach() // stale: must be a no-op for b
		mustWrite(t, o, "b-alive")
		select {
		case c, ok := <-b.C():
			if !ok || string(c) != "b-alive" {
				t.Fatalf("b disturbed by a stale Detach: %q ok=%v", c, ok)
			}
		default:
			t.Fatal("b got nothing")
		}
		b.Detach()
	})
}

// Under -race: writers, attachers, detachers and closers all at once. The only assertions are
// that nothing panics (double close, send on closed channel) and that every subscriber ends with
// its channel closed - both true at every interleaving.
func TestAdvConcurrentDetachCloseWriteDoNotPanic(t *testing.T) {
	for round := 0; round < 20; round++ {
		o := NewOutputBuffer(256)
		var subs []*Subscriber
		for i := 0; i < 16; i++ {
			_, s, err := o.Attach(64)
			if err != nil {
				t.Fatal(err)
			}
			subs = append(subs, s)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, s := range subs {
			for k := 0; k < 2; k++ {
				wg.Add(1)
				go func(s *Subscriber) {
					defer wg.Done()
					<-start
					s.Detach()
				}(s)
			}
		}
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < 50; i++ {
					_, _ = o.Write([]byte("w"))
				}
			}()
		}
		for c := 0; c < 3; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				o.Close()
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, s, err := o.Attach(64)
			if err == nil {
				s.Detach()
			}
		}()
		close(start)
		wg.Wait()
		for i, s := range subs {
			select {
			case _, ok := <-s.C():
				if ok {
					// drain the rest; it must end closed
					for range s.C() {
					}
				}
			default:
				t.Fatalf("round %d: subscriber %d channel open and empty after everyone detached and closed", round, i)
			}
		}
		if _, err := o.Write([]byte("x")); !errors.Is(err, ErrOutputClosed) {
			t.Fatalf("round %d: write after concurrent Close = %v", round, err)
		}
	}
}

// Under -race: a writer racing Detach must never send on the closed channel, and every chunk that
// does arrive before the close is intact and in order.
func TestAdvWriterRacingDetachDeliversOnlyWholeOrderedChunks(t *testing.T) {
	stream := advStream(4096)
	for round := 0; round < 50; round++ {
		o := NewOutputBuffer(1 << 16)
		_, sub, err := o.Attach(1 << 16)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i+4 <= len(stream); i += 4 {
				if _, err := o.Write(stream[i : i+4]); err != nil {
					return
				}
			}
		}()
		if round%2 == 0 {
			time.Sleep(20 * time.Microsecond)
		}
		sub.Detach()
		<-done
		// Whatever arrived is a contiguous prefix of the stream.
		var got bytes.Buffer
		for c := range sub.C() {
			got.Write(c)
		}
		if !bytes.HasPrefix(stream, got.Bytes()) {
			t.Fatalf("round %d: delivered bytes are not a prefix of the stream", round)
		}
		if got.Len()%4 != 0 {
			t.Fatalf("round %d: a partial chunk (%d bytes) was delivered", round, got.Len())
		}
	}
}

// Capacity <= 0 means the default, and the default must be what the constant says. A zero-size
// ring would make every Write take the "larger than the ring" path with a zero-length copy - and
// modulo by zero in append.
func TestAdvZeroAndNegativeCapacityUseTheDefault(t *testing.T) {
	for _, c := range []int{0, -1, -1 << 20} {
		o := NewOutputBuffer(c)
		if len(o.buf) != DefaultOutputBytes {
			t.Fatalf("NewOutputBuffer(%d): capacity %d, want %d", c, len(o.buf), DefaultOutputBytes)
		}
		mustWrite(t, o, "ok")
		if snap, at := o.Snapshot(); string(snap) != "ok" || at != 2 {
			t.Fatalf("NewOutputBuffer(%d): snapshot %q at %d", c, snap, at)
		}
	}
	// And queueBytes <= 0 on Attach likewise: the subscriber must have a real budget.
	o := NewOutputBuffer(16)
	_, sub, err := o.Attach(0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Detach()
	mustWrite(t, o, "budgeted")
	if sub.Lagged() {
		t.Fatal("Attach(0) produced a subscriber with no budget at all")
	}
}

// Snapshot on an empty buffer must return an empty, non-nil slice and offset 0 - and a snapshot of
// a buffer that has only ever received empty writes must say 0 written, not pretend activity.
func TestAdvEmptyBufferSnapshotIsEmptyNotNil(t *testing.T) {
	o := NewOutputBuffer(8)
	for i := 0; i < 3; i++ {
		if n, err := o.Write(nil); n != 0 || err != nil {
			t.Fatalf("Write(nil) = %d, %v", n, err)
		}
	}
	snap, at := o.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot returned nil; an empty buffer should be an empty slice, distinguishable from 'no answer'")
	}
	if len(snap) != 0 || at != 0 || o.Written() != 0 || o.Len() != 0 {
		t.Fatalf("empty buffer: snap=%q at=%d written=%d len=%d", snap, at, o.Written(), o.Len())
	}
	// Empty writes must not reach subscribers as empty chunks either - an empty chunk is
	// indistinguishable from "nothing" and would only confuse a reader counting bytes.
	_, sub, err := o.Attach(16)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Detach()
	if _, err := o.Write([]byte{}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-sub.C():
		t.Fatalf("empty write delivered a chunk of %d bytes", len(c))
	default:
	}
}

// advQueued reads the subscriber's internal byte count. In-package, so it can check the
// accounting directly rather than inferring it from Lagged().
func advQueued(s *Subscriber) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queued
}
