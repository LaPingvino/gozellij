package grid

import (
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt"
	"github.com/LaPingvino/gozellij/internal/vt/conform"
)

// A pty hands over whatever was in the buffer, so every case in the corpus must produce the same
// screen when it arrives one byte at a time as when it arrives whole.
func TestSplitInvariance(t *testing.T) {
	cases, err := conform.LoadCases(corpus)
	if err != nil {
		t.Fatal(err)
	}
	newTerm := func(cols, rows int) vt.Terminal { return New(cols, rows) }
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			for _, d := range conform.SplitInvariance(newTerm, c) {
				t.Errorf("%s", d)
			}
		})
	}
}
