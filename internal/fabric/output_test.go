package fabric

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOutputBufferKeepsEverythingWhenItFits(t *testing.T) {
	o := NewOutputBuffer(64)
	if _, err := o.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := o.Write([]byte("world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, at := o.Snapshot()
	if string(got) != "hello world" {
		t.Errorf("Snapshot = %q, want %q", got, "hello world")
	}
	if at != 11 {
		t.Errorf("offset = %d, want 11", at)
	}
	if o.Len() != 11 {
		t.Errorf("Len = %d, want 11", o.Len())
	}
}

// The ring must drop the *oldest* bytes and keep the newest, because the newest is what is on
// screen. Getting the wrap wrong shows up as a scrambled replay, so this walks it byte by byte.
func TestOutputBufferKeepsNewestWhenItWraps(t *testing.T) {
	o := NewOutputBuffer(8)
	for _, c := range "abcdefghijkl" { // 12 bytes into an 8 byte ring
		if _, err := o.Write([]byte{byte(c)}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got, at := o.Snapshot()
	if string(got) != "efghijkl" {
		t.Errorf("Snapshot = %q, want %q", got, "efghijkl")
	}
	if at != 12 {
		t.Errorf("offset = %d, want 12 (total ever written)", at)
	}
	if o.Written() != 12 {
		t.Errorf("Written = %d, want 12", o.Written())
	}
}

func TestOutputBufferChunkSpanningTheWrap(t *testing.T) {
	o := NewOutputBuffer(8)
	mustWrite(t, o, "abcdef") // start=0 size=6
	mustWrite(t, o, "ghijk")  // wraps, keeps last 8 of "abcdefghijk"
	got, _ := o.Snapshot()
	if string(got) != "defghijk" {
		t.Errorf("Snapshot = %q, want %q", got, "defghijk")
	}
}

func TestOutputBufferWriteLargerThanRing(t *testing.T) {
	o := NewOutputBuffer(4)
	mustWrite(t, o, "xy")
	mustWrite(t, o, "abcdefgh") // bigger than the whole ring
	got, _ := o.Snapshot()
	if string(got) != "efgh" {
		t.Errorf("Snapshot = %q, want %q (only the tail can survive)", got, "efgh")
	}
	if o.Len() != 4 {
		t.Errorf("Len = %d, want 4", o.Len())
	}
}

// The reason this type exists: a process whose output nobody reads must not block. Writes have to
// succeed with no reader attached, forever.
func TestWritesNeverBlockWithNoReaders(t *testing.T) {
	o := NewOutputBuffer(1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ {
			if _, err := o.Write([]byte("noise noise noise\n")); err != nil {
				t.Errorf("Write: %v", err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writing with no readers blocked - a child process would be wedged")
	}
}

// Attach must be atomic: a client cannot miss bytes that land between its snapshot and its
// subscription. Done as two calls this races, and it shows up as a rare corrupted screen.
func TestAttachHasNoGapBetweenSnapshotAndStream(t *testing.T) {
	o := NewOutputBuffer(1 << 16)
	mustWrite(t, o, "before;")

	// Write a bounded number of chunks, fewer than a subscriber's channel can hold, so that
	// "did not lag" is guaranteed by arithmetic rather than by the drainer winning a race. An
	// unthrottled writer will genuinely outrun any reader, and then lagging is correct
	// behaviour - testing the no-gap guarantee against that would be testing the scheduler.
	const chunks = 1000 // < subscriberChunks, and 1000 bytes << the 64 KiB byte budget
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < chunks; i++ {
			if _, err := o.Write([]byte("x")); err != nil {
				return
			}
			// Spread the writes out so the Attach below lands mid-stream, which is the
			// window this test exists to cover.
			time.Sleep(10 * time.Microsecond)
		}
	}()

	time.Sleep(2 * time.Millisecond)
	snap, sub, err := o.Attach(1 << 16)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Drain continuously. A subscriber nobody reads is *supposed* to fall behind and be marked
	// lagged, so leaving it undrained would test the wrong thing entirely - the no-gap guarantee
	// only holds for a reader that keeps up.
	var (
		live    bytes.Buffer
		liveMu  sync.Mutex
		drained = make(chan struct{})
	)
	go func() {
		defer close(drained)
		for chunk := range sub.C() {
			liveMu.Lock()
			live.Write(chunk)
			liveMu.Unlock()
			sub.Consumed(len(chunk))
		}
	}()

	wg.Wait()
	sub.Detach()
	<-drained

	if sub.Lagged() {
		t.Fatalf("subscriber lagged although only %d chunks were written into a %d-chunk channel",
			chunks, subscriberChunks)
	}
	liveMu.Lock()
	defer liveMu.Unlock()

	// snapshot + live stream must reconstruct a prefix-free, gap-free sequence: every byte
	// written is accounted for exactly once.
	total := int(o.Written())
	if got := len(snap) + live.Len(); got != total {
		t.Errorf("snapshot(%d) + live(%d) = %d bytes, but %d were written: there is a gap or an overlap",
			len(snap), live.Len(), got, total)
	}
	if !strings.HasPrefix(string(snap), "before;") {
		t.Errorf("snapshot does not start with the earlier output: %q", firstN(snap, 20))
	}
}

func TestSubscriberReceivesLiveOutput(t *testing.T) {
	o := NewOutputBuffer(1024)
	_, sub, err := o.Attach(1024)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	mustWrite(t, o, "live")

	select {
	case chunk := <-sub.C():
		if string(chunk) != "live" {
			t.Errorf("chunk = %q, want %q", chunk, "live")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber received nothing")
	}
}

// A reader that cannot keep up must be told, not quietly given an incomplete view. Design rule 1.
func TestSlowSubscriberIsMarkedLaggedRatherThanSilentlyLosingData(t *testing.T) {
	o := NewOutputBuffer(1 << 20)
	_, sub, err := o.Attach(128) // tiny budget: overflow almost immediately
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	for i := 0; i < 500; i++ {
		mustWrite(t, o, "0123456789")
	}
	if !sub.Lagged() {
		t.Error("subscriber fell far behind but was not marked lagged")
	}
	// ...and the writer was not blocked by it, which is the whole point.
	if o.Written() != 5000 {
		t.Errorf("Written = %d, want 5000", o.Written())
	}
}

func TestDetachClosesTheChannelAndIsIdempotent(t *testing.T) {
	o := NewOutputBuffer(1024)
	_, sub, err := o.Attach(1024)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	sub.Detach()
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Error("expected a closed channel after Detach")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was not closed by Detach")
	}
	sub.Detach() // must not panic on a double close
}

func TestCloseIsReportedNotIgnored(t *testing.T) {
	o := NewOutputBuffer(1024)
	_, sub, err := o.Attach(1024)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	o.Close()

	if _, ok := <-sub.C(); ok {
		t.Error("subscriber channel should be closed when the buffer closes")
	}
	if _, err := o.Write([]byte("x")); !errors.Is(err, ErrOutputClosed) {
		t.Errorf("Write after Close = %v, want ErrOutputClosed", err)
	}
	if _, _, err := o.Attach(1024); !errors.Is(err, ErrOutputClosed) {
		t.Errorf("Attach after Close = %v, want ErrOutputClosed", err)
	}
	o.Close() // idempotent
}

// Run with -race; this is the shape that actually happens: one PTY drain goroutine, clients
// attaching and detaching while it runs.
func TestConcurrentWritersAndAttachers(t *testing.T) {
	o := NewOutputBuffer(4096)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = o.Write([]byte("chatter "))
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, sub, err := o.Attach(512)
				if err != nil {
					return
				}
				go func() {
					for range sub.C() {
					}
				}()
				time.Sleep(time.Millisecond)
				sub.Detach()
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	o.Close()
}

func mustWrite(t *testing.T, o *OutputBuffer, s string) {
	t.Helper()
	if _, err := o.Write([]byte(s)); err != nil {
		t.Fatalf("Write(%q): %v", s, err)
	}
}

func firstN(b []byte, n int) string {
	if len(b) < n {
		n = len(b)
	}
	return string(b[:n])
}
