package fabric

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLog(t *testing.T, dir, file, body, index string) {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if index != "" {
		if err := os.WriteFile(IndexPath(p), []byte(index), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func minuteMarks(t *testing.T) {
	t.Helper()
	was := MarkEvery
	MarkEvery = time.Minute
	t.Cleanup(func() { MarkEvery = was })
}

func TestReadLogSinceStartsWhereTheOutputSinceThenBegins(t *testing.T) {
	minuteMarks(t)
	dir := t.TempDir()
	// Three bursts, marked as written at 100, 200 and 300 - each within a minute of its mark.
	writeLog(t, dir, "s.log", "AAAAAAAAAABBBBBBBBBBCCCCCCCCCC", "100 0\n200 10\n300 20\n")
	cases := []struct {
		since int64
		want  string
	}{
		{250, "BBBBBBBBBBCCCCCCCCCC"}, // inside the B burst's minute: from B, a little more
		{200, "BBBBBBBBBBCCCCCCCCCC"},
		{290, "CCCCCCCCCC"},                    // B's minute ended at 260: nothing of B is since 290
		{1000, ""},                             // the last burst ended by 360; nothing since 1000
		{50, "AAAAAAAAAABBBBBBBBBBCCCCCCCCCC"}, // older than anything kept: all of it
	}
	for _, c := range cases {
		got, err := ReadLogSince(dir, "s", time.Unix(c.since, 0), 0)
		if err != nil || string(got.Data) != c.want || got.Unknown != 0 {
			t.Errorf("since %d: %q unknown=%d, %v; want %q", c.since, got.Data, got.Unknown, err, c.want)
		}
	}
	got, _ := ReadLogSince(dir, "s", time.Unix(50, 0), 5)
	if string(got.Data) != "CCCCC" || !got.Truncated {
		t.Errorf("-n 5 gave %q truncated=%v", got.Data, got.Truncated)
	}
}

// A time that falls in the rotated generation takes the rest of it and all of the current file.
func TestReadLogSinceReachesIntoTheRotatedFile(t *testing.T) {
	minuteMarks(t)
	dir := t.TempDir()
	writeLog(t, dir, "s.log.1", "oldoldNEWER", "10 0\n200 6\n")
	writeLog(t, dir, "s.log", "current", "400 0\n")
	got, err := ReadLogSince(dir, "s", time.Unix(210, 0), 0)
	if err != nil || string(got.Data) != "NEWERcurrent" {
		t.Errorf("since 210: %q, %v", got.Data, err)
	}
	got, _ = ReadLogSince(dir, "s", time.Unix(5, 0), 0)
	if string(got.Data) != "oldoldNEWERcurrent" || got.Unknown != 0 {
		t.Errorf("since 5: %q unknown=%d", got.Data, got.Unknown)
	}
}

// Output written before the index began has no time. When since reaches back past the oldest
// mark it is included - leaving it out could hide recent output - but counted, so it is not
// passed off as filtered. Every log that existed before the index was introduced looks like this.
func TestBytesFromBeforeTheIndexAreCounted(t *testing.T) {
	minuteMarks(t)
	dir := t.TempDir()
	writeLog(t, dir, "s.log", "OLDOLDNEW", "100 6\n")
	got, err := ReadLogSince(dir, "s", time.Unix(50, 0), 0)
	if err != nil || string(got.Data) != "OLDOLDNEW" || got.Unknown != 6 {
		t.Errorf("since 50: %q unknown=%d, %v; want all of it with 6 unknown", got.Data, got.Unknown, err)
	}
	got, _ = ReadLogSince(dir, "s", time.Unix(120, 0), 0)
	if string(got.Data) != "NEW" || got.Unknown != 0 {
		t.Errorf("since 120: %q unknown=%d; want NEW", got.Data, got.Unknown)
	}
}

// A log with no index cannot answer -since, and must say so rather than print all of it as
// though it were "since" something.
func TestReadLogSinceRefusesALogWithNoIndex(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "s.log", "whatever", "")
	if _, err := ReadLogSince(dir, "s", time.Unix(1, 0), 0); !errors.Is(err, ErrNoIndex) {
		t.Errorf("got %v, want ErrNoIndex", err)
	}
}

// The writer marks as it goes, the offsets point at where each stretch begins, and rotation takes
// the index along with the file it indexes.
func TestTheLogWriterKeepsAnIndexThroughRotation(t *testing.T) {
	was := MarkEvery
	MarkEvery = 0
	t.Cleanup(func() { MarkEvery = was })

	dir := t.TempDir()
	out := NewOutputBuffer(1 << 16)
	path := LogPath(dir, "w")
	sink, err := NewLogSink(out, path, 64)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		out.Write([]byte("line-" + string(rune('a'+i)) + "\n"))
		time.Sleep(20 * time.Millisecond)
	}
	sink.Close()

	for _, p := range []string{path, path + ".1"} {
		body, _ := os.ReadFile(p)
		marks := readIndex(IndexPath(p))
		if len(marks) == 0 {
			t.Fatalf("%s has no index", p)
		}
		for i, m := range marks {
			if m.offset > int64(len(body)) {
				t.Fatalf("%s: a mark points past the end (%d > %d)", p, m.offset, len(body))
			}
			// Every write is marked here (MarkEvery is zero), so every mark must be further on
			// than the one before, and each must land where a write began.
			if i > 0 && m.offset <= marks[i-1].offset {
				t.Errorf("%s: mark %d is at %d, not after the previous one at %d", p, i, m.offset, marks[i-1].offset)
			}
			rest := string(body[m.offset:])
			if !strings.HasPrefix(rest, "line-") && !strings.HasPrefix(rest, "\r\n[gozellij]") {
				t.Errorf("%s: mark %d points into the middle of a write: %q", p, i, rest[:min(len(rest), 12)])
			}
		}
	}
	// The last mark in the current file starts a line of output.
	body, _ := os.ReadFile(path)
	marks := readIndex(IndexPath(path))
	last := marks[len(marks)-1]
	if !strings.HasPrefix(string(body[last.offset:]), "line-") {
		t.Errorf("the last mark points at %q, not the start of a write", body[last.offset:])
	}
}

// A rotation that fails leaves the same full file open. It used to carry on as though the file
// were empty - every later index mark short by its whole size - and to have moved the index to
// .1 beforehand, describing a rotated file that never existed.
func TestAFailedRotationKeepsTheIndexTrue(t *testing.T) {
	was := MarkEvery
	MarkEvery = 0
	t.Cleanup(func() { MarkEvery = was })

	dir := t.TempDir()
	path := LogPath(dir, "stuck")
	// The rename to .1 fails: a directory with something in it is in the way.
	if err := os.MkdirAll(filepath.Join(path+".1", "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := NewOutputBuffer(1 << 16)
	sink, err := NewLogSink(out, path, 200)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		out.Write([]byte("line-0123456\n"))
		time.Sleep(5 * time.Millisecond)
	}
	sink.Close()

	if _, err := os.Stat(IndexPath(path + ".1")); err == nil {
		t.Error("an index for a rotated file exists, and nothing was rotated")
	}
	body, _ := os.ReadFile(path)
	marks := readIndex(IndexPath(path))
	if len(marks) < 2 {
		t.Fatalf("only %d marks", len(marks))
	}
	for i, m := range marks {
		if m.offset > int64(len(body)) {
			t.Fatalf("mark %d points past the end (%d > %d)", i, m.offset, len(body))
		}
		if i > 0 && m.offset <= marks[i-1].offset {
			t.Errorf("mark %d at %d is not after mark %d at %d", i, m.offset, i-1, marks[i-1].offset)
		}
		if rest := string(body[m.offset:]); !strings.HasPrefix(rest, "line-") && !strings.HasPrefix(rest, "\r\n[gozellij]") {
			t.Errorf("mark %d points into the middle of a write: %q", i, rest[:min(12, len(rest))])
		}
	}
}
