package render

import (
	"os"
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt/conform"
	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

const corpus = "../conform/testdata"

// Rendering a screen and replaying it must produce the same screen.
//
// This is the property that makes a renderer worth trusting, and it is strong precisely because it
// is circular in a way that cannot hide a mistake: the emulator and the renderer would have to be
// wrong in exactly inverse ways to agree. A colour the renderer forgets to emit is a colour the
// emulator will not see; a wide character it draws in the wrong column lands in the wrong column.
//
// The screens it runs on are the corpus, which is to say screens a real terminal produced.
func TestRenderRoundTrips(t *testing.T) {
	cases, err := conform.LoadCases(corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			src := grid.New(c.Cols, c.Rows)
			play(t, src, c)

			cols, rows := src.Size()
			replayed := grid.New(cols, rows)
			if _, err := replayed.Write(Screen(src)); err != nil {
				t.Fatal(err)
			}

			// Scrollback is not part of a repaint: what is drawn is the visible screen, and the
			// replayed terminal has no history because nothing scrolled.
			want := conform.ScreenOf(src).WithoutHistory()
			got := conform.ScreenOf(replayed).WithoutHistory()

			for _, d := range conform.Diff(want, got) {
				t.Errorf("%s", d)
			}
		})
	}
}

func play(t *testing.T, term *grid.Term, c conform.Case) {
	t.Helper()
	steps := c.Steps
	if len(steps) == 0 {
		steps = []conform.Step{{Write: c.Input}}
	}
	for _, st := range steps {
		if st.IsResize() {
			if err := term.Resize(st.Cols, st.Rows); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := term.Write(st.Write); err != nil {
			t.Fatal(err)
		}
	}
}

// The round trip above is the renderer checked against the emulator, and both are mine. This is
// the one that is not: the rendered bytes are played into a real tmux, and the screen tmux ends up
// with must be the screen the case was recorded from in the first place.
//
// A renderer and an emulator that are wrong in inverse ways agree with each other perfectly. They
// cannot both fool tmux.
func TestRenderedBytesDrawTheSameScreenInTmux(t *testing.T) {
	cases, err := conform.LoadCases(corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			src := grid.New(c.Cols, c.Rows)
			play(t, src, c)
			cols, rows := src.Size()

			// The recording the case was made from, which is what a real terminal showed.
			b, err := os.ReadFile(conform.WantPath(corpus, c.Name))
			if err != nil {
				t.Fatal(err)
			}
			want, err := conform.ParseScreen(string(b))
			if err != nil {
				t.Fatal(err)
			}

			got, err := conform.Record(conform.Case{
				Name: c.Name + "-rendered",
				Cols: cols, Rows: rows,
				Input: Screen(src),
			})
			if err != nil {
				t.Fatal(err)
			}

			for _, d := range conform.Diff(want.WithoutHistory(), got.WithoutHistory()) {
				t.Errorf("%s", d)
			}
		})
	}
}
