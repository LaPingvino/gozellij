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

func TestReadLogSinceStartsAtTheLastMarkBeforeTheTime(t *testing.T) {
	dir := t.TempDir()
	// Three stretches of output, marked as written at 100, 200 and 300.
	writeLog(t, dir, "s.log", "AAAAAAAAAABBBBBBBBBBCCCCCCCCCC", "100 0\n200 10\n300 20\n")
	cases := []struct {
		since int64
		want  string
	}{
		{250, "BBBBBBBBBBCCCCCCCCCC"}, // from the 200 mark: a little more, never less
		{200, "BBBBBBBBBBCCCCCCCCCC"},
		{1000, "CCCCCCCCCC"},
		{50, "AAAAAAAAAABBBBBBBBBBCCCCCCCCCC"}, // older than anything kept: all of it
	}
	for _, c := range cases {
		got, _, err := ReadLogSince(dir, "s", time.Unix(c.since, 0), 0)
		if err != nil || string(got) != c.want {
			t.Errorf("since %d: %q, %v; want %q", c.since, got, err, c.want)
		}
	}
	got, truncated, _ := ReadLogSince(dir, "s", time.Unix(50, 0), 5)
	if string(got) != "CCCCC" || !truncated {
		t.Errorf("-n 5 gave %q truncated=%v", got, truncated)
	}
}

// A time that falls in the rotated generation takes the rest of it and all of the current file.
func TestReadLogSinceReachesIntoTheRotatedFile(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "s.log.1", "oldoldNEWER", "10 0\n20 6\n")
	writeLog(t, dir, "s.log", "current", "100 0\n")
	got, _, err := ReadLogSince(dir, "s", time.Unix(25, 0), 0)
	if err != nil || string(got) != "NEWERcurrent" {
		t.Errorf("since 25: %q, %v", got, err)
	}
	got, _, _ = ReadLogSince(dir, "s", time.Unix(5, 0), 0)
	if string(got) != "oldoldNEWERcurrent" {
		t.Errorf("since 5: %q", got)
	}
}

// A log with no index cannot answer -since, and must say so rather than print all of it as
// though it were "since" something.
func TestReadLogSinceRefusesALogWithNoIndex(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "s.log", "whatever", "")
	if _, _, err := ReadLogSince(dir, "s", time.Unix(1, 0), 0); !errors.Is(err, ErrNoIndex) {
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
