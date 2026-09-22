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
// It is not expected to be silent for ever. At the time of writing it still finds disagreements
// beyond the twelve seeds it runs by default - a background colour surviving on a blank cell where
// tmux drops it, and a cursor row after a run of scrolling sequences. Those are open, and saying so
// here is better than a comment claiming the emulator agrees with tmux in general when what is
// actually known is that it agrees on twenty-four recorded screens and the seeds below.
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
			if knownDivergence(small) {
				// Checked after shrinking, not before: the generator emits well-formed sequences
				// and it is the shrinker that produces this shape, by cutting a wide character
				// in half beside an escape. Checking the original stream skipped nothing and
				// reported the same known difference every time.
				t.Skipf("shrinks to the known-divergence class: %q", string(small))
			}
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

// knownDivergence reports streams where tmux does something this emulator deliberately does not.
//
// One case, and it is deliberate rather than unfinished: an escape followed by a byte of 0x80 or
// more makes tmux discard everything that comes after it, to the end of the stream. Measured -
// `\e\xaaX` leaves an empty screen and the cursor at home. That is tmux waiting for the end of
// something it will never find, and matching it would mean a garbled byte in a service's output
// being able to blank a user's pane and keep it blank. This emulator abandons the escape and
// carries on, which is the behaviour worth having even though it disagrees.
//
// The shrinker produces such streams readily by cutting a wide character in half next to an
// escape, so without this the generator reports the same known difference instead of finding new
// ones.
func knownDivergence(input []byte) bool {
	for i := 0; i+1 < len(input); i++ {
		if input[i] == 0x1b && input[i+1] >= 0x80 {
			return true
		}
	}
	return setsRegion(input) && shiftsLines(input)
}

// setsRegion and shiftsLines spot the second known divergence: inserting or deleting lines with
// the cursor outside a scrolling region.
//
// tmux has no rule there worth copying. On a six-row screen with a region over rows 3-6 and the
// cursor at the top, inserting one, two or three lines shifts the whole screen down as you would
// expect - and inserting four gives "||3C|4D|1A|2B|", which is not a shift of anything, while
// inserting six leaves the screen untouched. Rows rotate. This emulator shifts what fits and
// blanks the rest, which is coherent, and the corpus pins the in-region behaviour separately in
// regiondelete.
//
// The test is coarse - a stream that sets a region anywhere and shifts lines anywhere is skipped -
// and that costs real coverage. It is written this way because the alternative is teaching the
// generator to avoid one combination, which would hide the combination from every future run
// rather than from this one.
func setsRegion(input []byte) bool { return hasFinal(input, 'r') }
func shiftsLines(input []byte) bool {
	return hasFinal(input, 'L') || hasFinal(input, 'M')
}

// hasFinal reports whether the stream contains a CSI ending in the given byte.
func hasFinal(input []byte, final byte) bool {
	for i := 0; i+1 < len(input); i++ {
		if input[i] != 0x1b || input[i+1] != '[' {
			continue
		}
		for j := i + 2; j < len(input); j++ {
			if c := input[j]; c >= 0x40 && c <= 0x7e {
				if c == final {
					return true
				}
				break
			}
		}
	}
	return false
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
