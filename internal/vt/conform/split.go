package conform

import (
	"fmt"

	"github.com/LaPingvino/gozellij/internal/vt"
)

// SplitInvariance checks that an emulator does not care where a write ends.
//
// This is the one property the oracle cannot supply and the corpus cannot capture, and it is the
// one a pty violates all day: the kernel hands over whatever happened to be in the buffer, so a
// sequence arrives as "\x1b[1" and ";4r", a UTF-8 character arrives as two halves, and an emulator
// that only recognises whole things prints half a sequence as text. The recordings say what the
// screen should be; this says the screen must not depend on the accidents of buffering.
//
// A new terminal per splitting rather than one reused: the whole point is that state carried
// between writes is correct, and reusing one would carry state between *runs* as well.
//
// This is a consistency check, not a correctness one, and the distinction is not academic: the
// comparison is against the same emulator's whole-stream result, so a bug that is wrong the same
// way every time - truncating CSI parameters, say - is invisible here and shows up in the corpus
// instead. Measured, not assumed: that sabotage fails two corpus cases and no split-invariance
// case. The two checks cover different halves and neither is redundant.
func SplitInvariance(newTerm func(cols, rows int) vt.Terminal, c Case) []Difference {
	whole := newTerm(c.Cols, c.Rows)
	defer whole.Close()
	if _, err := whole.Write(c.Input); err != nil {
		return []Difference{{Row: -1, Col: -1, What: "writing the whole stream", Got: err.Error()}}
	}
	want := ScreenOf(whole)

	var diffs []Difference
	for _, size := range splitSizes {
		term := newTerm(c.Cols, c.Rows)
		for i := 0; i < len(c.Input); i += size {
			end := min(i+size, len(c.Input))
			if _, err := term.Write(c.Input[i:end]); err != nil {
				diffs = append(diffs, Difference{Row: -1, Col: -1,
					What: fmt.Sprintf("writing in chunks of %d", size), Got: err.Error()})
				break
			}
		}
		for _, d := range Diff(want, ScreenOf(term)) {
			d.What = fmt.Sprintf("in chunks of %d: %s", size, d.What)
			diffs = append(diffs, d)
		}
		term.Close()
	}
	return diffs
}

// splitSizes are the chunk sizes to try.
//
// One byte is the cruel one and the most valuable: every escape sequence and every multi-byte
// character is split at every possible point at once. The primes after it land the boundary
// somewhere different in each sequence rather than repeating the same alignment.
var splitSizes = []int{1, 2, 3, 5, 7, 13, 64}
