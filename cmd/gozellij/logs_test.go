package main

import (
	"bytes"
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
