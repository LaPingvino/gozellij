package conform

import (
	"flag"
	"os"
	"strings"
	"testing"
)

var record = flag.Bool("record", false, "re-record every case's .want file from tmux")

// TestCorpusMatchesTheOracle is the harness pointed at itself: every case is driven through a real
// tmux and compared against the recording checked in beside it. A disagreement means either tmux
// changed or the recording is wrong, and both are worth knowing before an emulator is written
// against these files.
//
// With -record it writes the recordings instead of comparing them. That is how the corpus is
// grown, and it is a separate flag rather than an automatic "write if missing" because a harness
// that records what it finds can never fail.
func TestCorpusMatchesTheOracle(t *testing.T) {
	cases, err := LoadCases(Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := Record(c)
			if err != nil {
				t.Fatalf("recording %s: %v", c.Name, err)
			}
			path := WantPath(Dir, c.Name)
			if *record {
				if err := os.WriteFile(path, []byte(got.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("recorded %s", path)
				return
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run `go test ./internal/vt/conform -record` to record it)", err)
			}
			want, err := ParseScreen(string(b))
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, d := range Diff(want, got) {
				t.Errorf("%s", d)
			}
		})
	}
}

// The corpus must not be able to pass by being empty, and a case must not be able to pass by
// having no recording.
func TestEveryCaseHasARecording(t *testing.T) {
	cases, err := LoadCases(Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 3 {
		t.Fatalf("only %d cases; the corpus is meant to grow, not shrink", len(cases))
	}
	for _, c := range cases {
		if _, err := os.Stat(WantPath(Dir, c.Name)); err != nil {
			t.Errorf("%s has no recording: %v", c.Name, err)
		}
	}
}

// ---------------------------------------------------------------- the comparator, watched failing

func screen(cursorRow, cursorCol int, lines ...string) Screen {
	return Screen{Cols: 20, Rows: len(lines), Lines: lines, CursorRow: cursorRow, CursorCol: cursorCol}
}

// One wrong cell must be reported at exactly that cell, and nothing else must be reported. A
// comparator that says "the screens differ" is not an oracle, it is a coin toss with extra steps.
func TestDiffNamesTheExactCell(t *testing.T) {
	want := screen(0, 0, "hello", "world")
	got := screen(0, 0, "hello", "wXrld")
	diffs := Diff(want, got)
	if len(diffs) != 1 {
		t.Fatalf("got %d differences, want 1: %v", len(diffs), diffs)
	}
	d := diffs[0]
	if d.Row != 1 || d.Col != 1 || d.Want != "o" || d.Got != "X" {
		t.Fatalf("got %v, want row 1 col 1 o/X", d)
	}
}

// The cursor is half of what a screen is, and an emulator that draws perfectly while leaving the
// cursor a row out is one that eats the line you typed on. That bug has already happened here once.
func TestDiffNoticesTheCursor(t *testing.T) {
	want := screen(2, 5, "hello")
	got := screen(2, 6, "hello")
	diffs := Diff(want, got)
	if len(diffs) != 1 || diffs[0].What != "cursor" {
		t.Fatalf("got %v, want one cursor difference", diffs)
	}
	if diffs[0].Want != "2,5" || diffs[0].Got != "2,6" {
		t.Fatalf("cursor difference does not say which: %v", diffs[0])
	}
}

// A wide character owns two columns, so a difference after one must be reported at the column it is
// drawn in - not at its offset in a UTF-8 string, which would be four columns adrift here.
func TestDiffCountsWideCharactersAsTwoColumns(t *testing.T) {
	want := screen(0, 0, "日本語 abc")
	got := screen(0, 0, "日本語 abX")
	diffs := Diff(want, got)
	if len(diffs) != 1 {
		t.Fatalf("got %d differences, want 1: %v", len(diffs), diffs)
	}
	if diffs[0].Col != 9 {
		t.Fatalf("difference reported at column %d, want 9 (three wide characters, a space, two letters)", diffs[0].Col)
	}
}

// A combining mark has no column of its own. Treating it as one would report every cell after an
// accented character as displaced, which would bury a real difference in noise.
func TestDiffTreatsACombiningMarkAsPartOfItsCharacter(t *testing.T) {
	// "e" + combining acute, then "x".
	if diffs := Diff(screen(0, 0, "éx"), screen(0, 0, "éx")); len(diffs) != 0 {
		t.Fatalf("identical screens differed: %v", diffs)
	}
	diffs := Diff(screen(0, 0, "éx"), screen(0, 0, "éy"))
	if len(diffs) != 1 || diffs[0].Col != 1 {
		t.Fatalf("got %v, want one difference at column 1", diffs)
	}
}

// A scroll moves everything, and the harness has to say so rather than pointing at one cell. This
// is the difference between a bug report and a hint.
func TestDiffReportsEveryDisagreeingCell(t *testing.T) {
	want := screen(0, 0, "one", "two", "three")
	got := screen(0, 0, "two", "three", "")
	diffs := Diff(want, got)
	if len(diffs) < 8 {
		t.Fatalf("a whole-screen shift produced only %d differences: %v", len(diffs), diffs)
	}
}

// Trailing blanks are the oracle's own trimming. If they were compared, every case would fail on
// whether tmux felt like padding a row.
func TestDiffIgnoresTrailingBlanks(t *testing.T) {
	if diffs := Diff(screen(0, 0, "hi"), screen(0, 0, "hi    ")); len(diffs) != 0 {
		t.Fatalf("trailing spaces were compared: %v", diffs)
	}
}

// A missing row is not a silent pass.
func TestDiffNoticesAMissingRow(t *testing.T) {
	want := Screen{Cols: 20, Rows: 2, Lines: []string{"a", "b"}}
	got := Screen{Cols: 20, Rows: 2, Lines: []string{"a"}}
	diffs := Diff(want, got)
	if len(diffs) != 1 || !strings.Contains(diffs[0].What, "missing") {
		t.Fatalf("got %v, want a missing-row difference", diffs)
	}
}

// ------------------------------------------------------------------------------ the file format

func TestScreenRoundTrips(t *testing.T) {
	want := Screen{Cols: 20, Rows: 3, Lines: []string{"hello", "", " leading space"}, CursorRow: 2, CursorCol: 7}
	got, err := ParseScreen(want.String())
	if err != nil {
		t.Fatal(err)
	}
	if diffs := Diff(want, got); len(diffs) != 0 {
		t.Fatalf("round trip changed the screen: %v", diffs)
	}
	if got.Cols != want.Cols || got.Rows != want.Rows {
		t.Fatalf("round trip lost the size: %dx%d", got.Cols, got.Rows)
	}
}

// A recording with no header would compare as a 0x0 screen, which reports a difference about the
// wrong thing entirely.
func TestParseScreenRejectsAHeaderlessRecording(t *testing.T) {
	if _, err := ParseScreen("row hello\n"); err == nil {
		t.Fatal("a recording with no cols/rows/cursor was accepted")
	}
}

func TestUnescape(t *testing.T) {
	got, err := Unescape("# a comment\n\\e[1;4r\\r\\n\\x41\\\\")
	if err != nil {
		t.Fatal(err)
	}
	if want := "\x1b[1;4r\r\nA\\"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A typo like \E must not quietly become "E": the case would then test something other than what
// it says, and pass.
func TestUnescapeRejectsAnUnknownEscape(t *testing.T) {
	if _, err := Unescape("\\E[1m"); err == nil {
		t.Fatal("\\E was accepted")
	}
}

func TestLoadCaseReadsTheSize(t *testing.T) {
	c, err := LoadCase(Dir + "/scroll.in")
	if err != nil {
		t.Fatal(err)
	}
	if c.Cols != 20 || c.Rows != 5 {
		t.Fatalf("got %dx%d, want 20x5", c.Cols, c.Rows)
	}
	if !strings.HasPrefix(string(c.Input), "\x1b[1;4r") {
		t.Fatalf("the comment lines were not stripped: %q", c.Input)
	}
}

// The whole path at once: a real recording, one cell changed, and the disagreement found at that
// cell and nowhere else. The comparator tests above use screens made up in the test, so this is
// the one that says tmux, the file format and the diff agree about what a column is.
func TestACorruptedRecordingIsCaughtAtTheRightCell(t *testing.T) {
	c, err := LoadCase(Dir + "/wrap.in")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(WantPath(Dir, c.Name))
	if err != nil {
		t.Fatal(err)
	}
	want, err := ParseScreen(string(b))
	if err != nil {
		t.Fatal(err)
	}
	// Row 1 column 3 of the recording, replaced with something that is not there.
	row := []rune(want.Lines[1])
	if len(row) < 4 {
		t.Fatalf("the recording is not what this test assumes: %q", want.Lines[1])
	}
	row[3] = 'Z'
	want.Lines[1] = string(row)

	got, err := Record(c)
	if err != nil {
		t.Fatal(err)
	}
	diffs := Diff(want, got)
	if len(diffs) != 1 {
		t.Fatalf("got %d differences, want exactly 1: %v", len(diffs), diffs)
	}
	if diffs[0].Row != 1 || diffs[0].Col != 3 || diffs[0].Want != "Z" {
		t.Fatalf("got %v, want row 1 col 3 expecting Z", diffs[0])
	}
}

// And the cursor, the same way: a recording that is one column out must fail against the oracle.
func TestACorruptedCursorIsCaught(t *testing.T) {
	c, err := LoadCase(Dir + "/scroll.in")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(WantPath(Dir, c.Name))
	if err != nil {
		t.Fatal(err)
	}
	want, err := ParseScreen(string(b))
	if err != nil {
		t.Fatal(err)
	}
	want.CursorRow++

	got, err := Record(c)
	if err != nil {
		t.Fatal(err)
	}
	diffs := Diff(want, got)
	if len(diffs) != 1 || diffs[0].What != "cursor" {
		t.Fatalf("got %v, want one cursor difference", diffs)
	}
}

// A recording must stay a text file even when the screen contains a control byte - a terminal
// reply that a capture echoed back, for instance. Writing the raw byte would make the file
// something a pager mangles and a reviewer skims.
func TestRecordingsEscapeControlBytes(t *testing.T) {
	want := Screen{Cols: 20, Rows: 1, Lines: []string{"a\x1b[2Rb\\c"}}
	text := want.String()
	if strings.ContainsAny(text, "\x1b") {
		t.Fatalf("a raw escape byte was written to the recording: %q", text)
	}
	got, err := ParseScreen(text)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lines[0] != want.Lines[0] {
		t.Fatalf("round trip changed the row: %q -> %q", want.Lines[0], got.Lines[0])
	}
}
