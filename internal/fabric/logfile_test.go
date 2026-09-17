package fabric

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForFile polls until the file contains want, or gives up. The sink writes on its own
// goroutine, so "write then read" needs a moment; polling beats a sleep that is either flaky or
// slow.
func waitForFile(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			last = string(b)
			if strings.Contains(last, want) {
				return last
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waiting for %q in %s; have %q", want, path, last)
	return ""
}

func TestLogSinkWritesWhatTheProcessWrote(t *testing.T) {
	dir := t.TempDir()
	path := LogPath(dir, "web")

	out := NewOutputBuffer(1024)
	sink, err := NewLogSink(out, path, 0)
	if err != nil {
		t.Fatalf("NewLogSink: %v", err)
	}
	defer sink.Close()

	// Escape sequences and all: the log is what the child wrote, not a cleaned-up version of it.
	if _, err := out.Write([]byte("hello\033[2J world")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := waitForFile(t, path, "hello")
	// Past the header each daemon writes when it opens the file, the bytes are the child's own:
	// escape sequences and all, because that is what it wrote.
	if _, body, ok := strings.Cut(got, "---\r\n"); !ok || body != "hello\033[2J world" {
		t.Errorf("log = %q, want a header then the bytes verbatim", got)
	}
	if e := out.LogError(); e != "" {
		t.Errorf("LogError = %q, want none", e)
	}
	if out.LogPath() != path {
		t.Errorf("LogPath = %q, want %q", out.LogPath(), path)
	}
}

func TestLogIsNotReadableByOthers(t *testing.T) {
	// The log of a shell contains everything you typed at it and everything it answered.
	dir := t.TempDir()
	path := LogPath(dir, "shell")
	out := NewOutputBuffer(1024)
	sink, err := NewLogSink(out, path, 0)
	if err != nil {
		t.Fatalf("NewLogSink: %v", err)
	}
	defer sink.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("log mode = %04o, want no group/other access", mode)
	}
}

func TestEachDaemonMarksWhereItsRunBegins(t *testing.T) {
	// Two runs of a service that prints the same line must not read as one run that printed it
	// twice. Measured by hand before this line existed, and it was genuinely confusing.
	dir := t.TempDir()
	path := LogPath(dir, "web")

	out := NewOutputBuffer(1024)
	sink, err := NewLogSink(out, path, 0)
	if err != nil {
		t.Fatalf("NewLogSink: %v", err)
	}
	defer sink.Close()

	got := waitForFile(t, path, "log opened by daemon pid")
	if !strings.Contains(got, "web.log") {
		t.Errorf("marker = %q, want it to name the log", got)
	}
}

func TestLogSurvivesTheDaemonThatWroteIt(t *testing.T) {
	// This is the whole point of the feature: the ring dies with the process, the file does not.
	dir := t.TempDir()
	path := LogPath(dir, "web")

	first := NewOutputBuffer(1024)
	sink, err := NewLogSink(first, path, 0)
	if err != nil {
		t.Fatalf("NewLogSink: %v", err)
	}
	first.Write([]byte("before the restart\n"))
	waitForFile(t, path, "before the restart")
	sink.Close()
	first.Close()

	// A new daemon: new buffer, new sink, same file.
	second := NewOutputBuffer(1024)
	sink2, err := NewLogSink(second, path, 0)
	if err != nil {
		t.Fatalf("NewLogSink after restart: %v", err)
	}
	defer sink2.Close()
	second.Write([]byte("after the restart\n"))
	got := waitForFile(t, path, "after the restart")

	if !strings.Contains(got, "before the restart") {
		t.Errorf("log = %q, want the pre-restart output kept (O_APPEND, not O_TRUNC)", got)
	}
}

func TestLogRotatesAndKeepsTheTail(t *testing.T) {
	dir := t.TempDir()
	path := LogPath(dir, "noisy")

	out := NewOutputBuffer(4096)
	sink, err := NewLogSink(out, path, 64)
	if err != nil {
		t.Fatalf("NewLogSink: %v", err)
	}
	defer sink.Close()

	for i := 0; i < 20; i++ {
		if _, err := out.Write([]byte(fmt.Sprintf("line %02d ............\n", i))); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	waitForFile(t, path, "line 19")

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated generation missing: %v", err)
	}

	// The newest output is still there, which is what rotation is for.
	data, truncated, err := ReadLogTail(dir, "noisy", 0)
	if err != nil {
		t.Fatalf("ReadLogTail: %v", err)
	}
	if !strings.Contains(string(data), "line 19") {
		t.Errorf("tail = %q, want the newest line", data)
	}
	if truncated {
		t.Errorf("truncated = true when asked for everything in both generations")
	}

	// And the rotated generation is read too, rather than silently answering with only what the
	// current file happens to hold.
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) <= len(current) {
		t.Errorf("tail is %d bytes and the current file is %d; the rotated generation was not read",
			len(data), len(current))
	}
}

func TestReadLogTailSaysWhenItLeftSomethingOut(t *testing.T) {
	dir := t.TempDir()
	path := LogPath(dir, "web")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	data, truncated, err := ReadLogTail(dir, "web", 4)
	if err != nil {
		t.Fatalf("ReadLogTail: %v", err)
	}
	if string(data) != "6789" {
		t.Errorf("tail = %q, want the last four bytes", data)
	}
	if !truncated {
		t.Error("truncated = false, but six bytes were left out")
	}

	data, truncated, err = ReadLogTail(dir, "web", 0)
	if err != nil {
		t.Fatalf("ReadLogTail: %v", err)
	}
	if string(data) != "0123456789" || truncated {
		t.Errorf("tail = %q truncated = %v, want everything and not truncated", data, truncated)
	}
}

func TestReadLogTailOfAServiceWithNoLog(t *testing.T) {
	_, _, err := ReadLogTail(t.TempDir(), "nothing", 0)
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want ErrNotExist so the caller knows to fall back to the ring", err)
	}
}

// TestCatchUpDoesNotDuplicateOrInventOutput exercises the offset arithmetic used when the sink
// falls behind and has to re-subscribe. Getting this wrong writes the same bytes twice, which is
// worse than a gap because a duplicate is invisible.
func TestCatchUpDoesNotDuplicateOrInventOutput(t *testing.T) {
	cases := []struct {
		name    string
		flushed int64
		snap    string
		at      int64
		want    string
		wantGap bool
	}{
		{
			name:    "snapshot entirely already written",
			flushed: 10,
			snap:    "abcdefghij", // covers [0,10)
			at:      10,
			want:    "",
		},
		{
			name:    "snapshot overlaps what we have",
			flushed: 6,
			snap:    "abcdefghij", // covers [0,10)
			at:      10,
			want:    "ghij",
		},
		{
			name:    "the ring wrapped past us",
			flushed: 2,
			snap:    "klmno", // covers [10,15): bytes 2..10 are gone for good
			at:      15,
			want:    "klmno",
			wantGap: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "x.log")
			f, err := openLog(path)
			if err != nil {
				t.Fatalf("openLog: %v", err)
			}
			l := &LogSink{path: path, f: f, flushed: tc.flushed}
			l.catchUp([]byte(tc.snap), tc.at)
			f.Close()

			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			got := string(b)

			note := ""
			if i := strings.Index(got, "[gozellij]"); i >= 0 {
				j := strings.Index(got[i:], "\r\n")
				note = got[i : i+j+2]
				got = strings.Replace(got, "\r\n"+note, "", 1)
			}
			if got != tc.want {
				t.Errorf("wrote %q, want %q", got, tc.want)
			}
			if tc.wantGap && note == "" {
				t.Error("no note about the bytes that were lost; a silent gap is exactly what design rule 1 forbids")
			}
			if !tc.wantGap && note != "" {
				t.Errorf("wrote a gap note %q when nothing was lost", note)
			}
			if tc.wantGap && !strings.Contains(note, "8 bytes") {
				t.Errorf("note = %q, want it to say how many bytes went missing (8)", note)
			}
			if l.flushed != tc.at {
				t.Errorf("flushed = %d, want %d", l.flushed, tc.at)
			}
		})
	}
}

func TestSupervisorReportsALogItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	// Something is already in the way, and it is not a file we can append to.
	if err := os.MkdirAll(LogPath(dir, "web"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	sup := NewSupervisor(Service{Name: "web", Command: "true"}, StartOptions{LogDir: dir})
	st := sup.Status()
	if st.LogError == "" {
		t.Fatal("LogError is empty; a log that cannot be opened must be said out loud, not discovered later by its absence")
	}
	if !strings.Contains(st.LogError, "web.log") {
		t.Errorf("LogError = %q, want it to name the file", st.LogError)
	}
}

func TestServiceWithLoggingOffWritesNoFile(t *testing.T) {
	dir := t.TempDir()
	sup := NewSupervisor(Service{Name: "shell", Command: "true", NoLog: true}, StartOptions{LogDir: dir})
	sup.Output().Write([]byte("secret\n"))

	if _, err := os.Stat(LogPath(dir, "shell")); !os.IsNotExist(err) {
		t.Errorf("Stat = %v, want no file at all for a service with logging off", err)
	}
	if e := sup.Status().LogError; e != "" {
		t.Errorf("LogError = %q, but not logging on purpose is a choice, not a fault", e)
	}
}

func TestOneSinkPerBufferAcrossSupervisorReplacement(t *testing.T) {
	// Fabric.replaceSupervisor hands the buffer to a fresh supervisor. If that supervisor opened
	// a second sink on the same file, every byte would be written twice - invisible corruption.
	dir := t.TempDir()
	svc := Service{Name: "web", Command: "true"}

	first := NewSupervisor(svc, StartOptions{LogDir: dir})
	opts := StartOptions{LogDir: dir, Output: first.Output()}
	second := NewSupervisor(svc, opts)

	if second.Output() != first.Output() {
		t.Fatal("successor did not reuse the buffer; the test is not testing what it thinks")
	}
	second.Output().Write([]byte("once\n"))

	got := waitForFile(t, LogPath(dir, "web"), "once")
	if n := strings.Count(got, "once"); n != 1 {
		t.Errorf("log = %q, wrote the line %d times, want 1", got, n)
	}
}
