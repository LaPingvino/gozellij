package conform

import (
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt"
)

// The oracle's own arithmetic, tested directly.
//
// Five of the twenty bugs this harness has found were in here rather than in the emulator: a tab
// expanded by byte length, a tab expanded past the last column, styles parsed without carrying
// across rows, a tab counted as one column in the styled parse, and a pty rewriting newlines
// before tmux saw them. Each was found by the emulator being blamed for it, which is an expensive
// way to test a helper function.
//
// These are the checks that would have caught them a step earlier.

func TestExpandTabsCountsColumnsNotBytes(t *testing.T) {
	// A replacement character is three bytes and one column, so the tab after it must reach
	// column 8, not column 8 counted in bytes.
	got := expandTabs([]string{"�\t@"}, 40)[0]
	if want := "�       @"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if n := vt.StringWidth(got); n != 9 {
		t.Fatalf("the row is %d columns wide, want 9", n)
	}
}

func TestExpandTabsStopsAtTheLastColumn(t *testing.T) {
	// From column 8 on a twelve-column screen the next stop is 16, which is off the screen: the
	// cursor stops at the last column, so the mark after it lands in column 11.
	got := expandTabs([]string{"12345678\tZ"}, 12)[0]
	if n := vt.StringWidth(got); n > 12 {
		t.Fatalf("the row is %d columns wide on a twelve-column screen: %q", n, got)
	}
	if !strings.HasSuffix(got, "Z") {
		t.Fatalf("got %q, want it to end with the mark", got)
	}
}

func TestExpandTabsLeavesRowsWithoutTabsAlone(t *testing.T) {
	in := []string{"plain", "also plain"}
	got := expandTabs(append([]string(nil), in...), 40)
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("row %d changed from %q to %q", i, in[i], got[i])
		}
	}
}

// A style stays in force until something changes it, including across rows: tmux emits the
// sequences needed to reproduce a screen, not a dump of cells.
func TestStyledScreenCarriesStyleAcrossRows(t *testing.T) {
	rows := styledScreen([]string{"\x1b[94m~", "~", "~"})
	for i, row := range rows {
		if len(row) != 1 {
			t.Fatalf("row %d has %d columns, want 1: %v", i, len(row), row)
		}
		if row[0] != "12" {
			t.Fatalf("row %d is %q, want the bright blue set on the first row to still apply", i, row[0])
		}
	}
}

// A tab in the styled capture stands for the cells the cursor skipped.
func TestStyledColumnsExpandsATab(t *testing.T) {
	var style vt.Style
	got := styledColumns("\tC", &style)
	if len(got) != 9 {
		t.Fatalf("got %d columns, want 9 (eight skipped and the C): %v", len(got), got)
	}
	if got[8] != "-" {
		t.Fatalf("the C has style %q, want the default", got[8])
	}
}

// Reset must clear everything, not only what the next sequence happens to mention.
func TestStyledColumnsReset(t *testing.T) {
	var style vt.Style
	got := styledColumns("\x1b[1;4;31mX\x1b[0mY", &style)
	if len(got) != 2 {
		t.Fatalf("got %v, want two columns", got)
	}
	if got[0] == "-" {
		t.Fatal("the styled character came back unstyled")
	}
	if got[1] != "-" {
		t.Fatalf("after a reset the style is %q, want the default", got[1])
	}
}

// What a blank cell shows: not a foreground colour, but a background, an underline, a reverse.
func TestVisibleOnBlank(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"31", "-"},     // a foreground on a space draws nothing
		{"b", "-"},      // nor does bold
		{"bi31", "-"},   // nor several of them together
		{"u", "u"},      // an underline draws a line
		{"r", "r"},      // reverse fills the cell
		{"^4", "^4"},    // so does a background
		{"b31^4", "^4"}, // the background survives, the rest does not
		{"u^4", "^4u"},  // both, in a stable order
		{"-", "-"},      // and the default stays the default
	} {
		if got := visibleOnBlank(c.in); got != c.want {
			t.Errorf("visibleOnBlank(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A wide character owns two display columns; a combining mark owns none.
func TestColumnsByDisplayPosition(t *testing.T) {
	got := columns("a日b")
	if len(got) != 4 {
		t.Fatalf("got %d columns for a wide character between two letters: %v", len(got), got)
	}
	if got[1] != "日" || got[2] != "" {
		t.Fatalf("the wide character occupies %q and %q, want it followed by an empty column", got[1], got[2])
	}
	if mark := columns("éx"); len(mark) != 2 {
		t.Fatalf("a combining mark took a column of its own: %v", mark)
	}
}
