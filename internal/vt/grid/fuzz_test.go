package grid

import (
	"flag"
	"fmt"
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt/conform"
)

var (
	fuzzCases = flag.Int("fuzz", 12, "how many generated streams to check against tmux")
	fuzzSeed  = flag.Int64("fuzzseed", 1, "the first seed to use")
)

// TestAgainstTmux drives generated streams into this emulator and into a real tmux, and requires
// them to agree.
//
// The corpus tests what somebody thought of. This tests what nobody thought of, which is the
// arrangement DESIGN.md asks for. A disagreement is shrunk and printed as a ready-made corpus
// case, because a fuzzer that only says "these differ" leaves the hard half of the work undone.
//
// Twelve streams by default, which is a few seconds - enough that a regression in an ordinary
// sequence is caught by an ordinary test run. Run many more deliberately:
//
//	go test ./internal/vt/grid -run TestAgainstTmux -fuzz 500 -fuzzseed 1000 -timeout 30m
func TestAgainstTmux(t *testing.T) {
	const cols, rows = 40, 8

	disagrees := func(input []byte) bool {
		return len(differences(t, input, cols, rows)) > 0
	}

	for i := 0; i < *fuzzCases; i++ {
		seed := *fuzzSeed + int64(i)
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			input := conform.Generate(seed, cols, rows)
			diffs := differences(t, input, cols, rows)
			if len(diffs) == 0 {
				return
			}
			small := conform.Shrink(input, disagrees)
			// The shrunk stream's own differences, not the original's. Reporting the original's
			// beside the shrunk input named a cell that the shrunk input never writes, which sent
			// the first investigation looking for a character that was not there.
			smallDiffs := differences(t, small, cols, rows)
			if len(smallDiffs) == 0 {
				smallDiffs = diffs
			}
			why := fmt.Sprintf("found by TestAgainstTmux at seed %d: %s", seed, smallDiffs[0])
			t.Errorf("this emulator and tmux disagree:\n  %q\n  first of %d differences: %s\n\nas a corpus case:\n%s",
				string(small), len(smallDiffs), smallDiffs[0], conform.AsCase(small, cols, rows, why))
		})
	}
}

// differences records one input through tmux and plays it through this emulator, and compares.
func differences(t *testing.T, input []byte, cols, rows int) []conform.Difference {
	t.Helper()
	c := conform.Case{Name: "generated", Cols: cols, Rows: rows, Input: input}
	want, err := conform.Record(c)
	if err != nil {
		t.Fatalf("recording: %v", err)
	}
	term := New(cols, rows)
	if _, err := term.Write(input); err != nil {
		t.Fatalf("writing: %v", err)
	}
	// Scrollback is not compared here. tmux and this emulator disagree about what a scrolling
	// region contributes to history - deliberately, and recorded in the corpus as scrollregion -
	// and a generator that sets regions constantly would report that one known difference over and
	// over instead of finding new ones.
	return conform.Diff(want.WithoutHistory(), conform.ScreenOf(term).WithoutHistory())
}
