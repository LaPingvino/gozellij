package grid

import (
	"os"
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt/conform"
)

const corpus = "../conform/testdata"

// TestCorpus grades this emulator against the recordings taken from tmux.
//
// Not "does it look right" - a case is a screen a real terminal produced, and every disagreeing
// cell is named. Cases this emulator is not expected to pass yet are listed in `unsupported` with
// the reason; they still run, and a case that starts passing is reported, because an exception
// nobody revisits is how a limitation becomes permanent.
func TestCorpus(t *testing.T) {
	cases, err := conform.LoadCases(corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			b, err := os.ReadFile(conform.WantPath(corpus, c.Name))
			if err != nil {
				t.Fatal(err)
			}
			want, err := conform.ParseScreen(string(b))
			if err != nil {
				t.Fatal(err)
			}

			term := New(c.Cols, c.Rows)
			if err := play(term, c); err != nil {
				t.Fatal(err)
			}
			diffs := conform.Diff(want, conform.ScreenOf(term))

			reason, expected := unsupported[c.Name]
			switch {
			case expected && len(diffs) == 0:
				t.Errorf("this case is listed as unsupported (%s) but passes; remove it from the list", reason)
			case expected:
				t.Logf("known to fail (%s): %d differences, first: %v", reason, len(diffs), diffs[0])
			default:
				for _, d := range diffs {
					t.Errorf("%s", d)
				}
			}
		})
	}
}

// play runs a case's steps in order: write these bytes, change to that size, write those.
func play(term *Term, c conform.Case) error {
	steps := c.Steps
	if len(steps) == 0 {
		steps = []conform.Step{{Write: c.Input}}
	}
	for _, st := range steps {
		if st.IsResize() {
			if err := term.Resize(st.Cols, st.Rows); err != nil {
				return err
			}
			continue
		}
		if _, err := term.Write(st.Write); err != nil {
			return err
		}
	}
	return nil
}

// unsupported names what this emulator cannot do yet and why, so that a red case is a decision
// rather than a surprise. Every line here is a to-do with a test already written for it.
var unsupported = map[string]string{}
