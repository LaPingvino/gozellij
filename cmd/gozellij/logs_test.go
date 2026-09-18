package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

// A single service must come through byte-identical. `logs -f x | grep` and a service that draws
// with escape sequences both depend on it, and prefixing one stream would be a change nobody asked
// for.
func TestPrefixWriterSingleServiceIsUntouched(t *testing.T) {
	var buf bytes.Buffer
	out := &prefixedOutput{w: &buf}
	w := out.for_("")
	w.Write([]byte("a\nb"))
	w.Write([]byte("\x1b[2Jc\n"))
	if got, want := buf.String(), "a\nb\x1b[2Jc\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The prefix goes at the start of every line, including when a chunk arrives split across the
// newline - which is the ordinary case for a pipe, not an edge one. A writer that prefixed each
// Write call instead would put the name in the middle of a line and leave the next line bare.
func TestPrefixWriterSplitAcrossNewline(t *testing.T) {
	var buf bytes.Buffer
	out := &prefixedOutput{w: &buf}
	w := out.for_("svc | ")
	w.Write([]byte("hel"))
	w.Write([]byte("lo\nwor"))
	w.Write([]byte("ld\n"))
	if got, want := buf.String(), "svc | hello\nsvc | world\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A trailing newline must not print a prefix with nothing after it: the last line of a log that
// ends properly would otherwise be followed by a dangling "svc | ".
func TestPrefixWriterNoTrailingPrefix(t *testing.T) {
	var buf bytes.Buffer
	out := &prefixedOutput{w: &buf}
	out.for_("svc | ").Write([]byte("done\n"))
	if got, want := buf.String(), "svc | done\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A service that writes while another is mid-line must not continue that line. Half of one
// service's output and half of another's under a single name is a lie the prefix is there to
// prevent, so the unfinished line is broken instead.
func TestPrefixWriterBreaksAnotherServicesUnfinishedLine(t *testing.T) {
	var buf bytes.Buffer
	out := &prefixedOutput{w: &buf}
	a, b := out.for_("a | "), out.for_("b | ")
	a.Write([]byte("one"))
	b.Write([]byte("two\n"))
	a.Write([]byte("-more\n"))
	if got, want := buf.String(), "a | one\nb | two\na | -more\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Concurrent writers must not tear a line apart. Run under -race; without the lock this is a data
// race on the map as well as scrambled output.
func TestPrefixWriterConcurrentLinesStayWhole(t *testing.T) {
	var buf bytes.Buffer
	out := &prefixedOutput{w: &buf}
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			w := out.for_(name + " | ")
			for i := 0; i < 100; i++ {
				w.Write([]byte("0123456789\n"))
			}
		}(name)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 300 {
		t.Fatalf("got %d lines, want 300", len(lines))
	}
	for _, l := range lines {
		if len(l) != len("a | 0123456789") {
			t.Fatalf("torn line %q", l)
		}
	}
}

// eachService must keep going past a failure. Stopping at the first one would leave the services
// after it untouched while the command exits non-zero, which reads as "nothing happened" and is
// not what happened.
func TestEachServiceKeepsGoingPastAFailure(t *testing.T) {
	var seen []string
	err := eachService([]string{"a", "b", "c"}, func(name string) error {
		seen = append(seen, name)
		if name == "b" {
			return errors.New("no such service")
		}
		return nil
	})
	if err == nil {
		t.Fatal("a failure was not reported")
	}
	if got, want := strings.Join(seen, ","), "a,b,c"; got != want {
		t.Fatalf("visited %q, want %q", got, want)
	}
	if !strings.Contains(err.Error(), "1 of 3") {
		t.Fatalf("error does not say how many failed: %v", err)
	}
}

// A single name must return the operation's own error untouched. Wrapping it in "1 of 1 services
// failed" would change what every existing one-name invocation prints.
func TestEachServiceSingleNameReturnsTheErrorItself(t *testing.T) {
	want := errors.New("service.stop: no such service: nosuch")
	got := eachService([]string{"nosuch"}, func(string) error { return want })
	if got != want {
		t.Fatalf("got %v, want the original error", got)
	}
}

// All of them succeeding is not an error, and all of them failing must still be counted rather
// than reported as the last one.
func TestEachServiceCounts(t *testing.T) {
	if err := eachService([]string{"a", "b"}, func(string) error { return nil }); err != nil {
		t.Fatalf("two successes reported an error: %v", err)
	}
	err := eachService([]string{"a", "b"}, func(string) error { return errors.New("boom") })
	if err == nil || !strings.Contains(err.Error(), "2 of 2") {
		t.Fatalf("got %v, want a count of 2 of 2", err)
	}
}
