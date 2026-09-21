package conform

import (
	"fmt"
	"math/rand"
	"strings"
)

// Generating input is the half of the oracle the corpus cannot be.
//
// A hand-written case tests what somebody thought of. This produces streams nobody thought of and
// asks a real terminal what they mean, which is the arrangement DESIGN.md describes: drive the same
// bytes into two implementations, diff the result, and treat every disagreement as a bug report
// that wrote itself.
//
// The generator is weighted rather than uniform. Uniformly random bytes are almost all printable
// text and almost never a sequence a terminal does anything interesting with, so the interesting
// disagreements would be found at a rate of roughly never. This emits text, control characters and
// CSI sequences in proportions that keep the cursor moving and the screen scrolling.

// Generate makes one pseudo-random input, reproducible from its seed.
//
// The seed is part of the result rather than hidden inside it, because a disagreement is only
// useful if it can be produced again on demand.
func Generate(seed int64, cols, rows int) []byte {
	r := rand.New(rand.NewSource(seed))
	var b strings.Builder
	for i := 0; i < 40+r.Intn(60); i++ {
		switch n := r.Intn(100); {
		case n < 35:
			b.WriteString(randomText(r))
		case n < 45:
			b.WriteString("\r\n")
		case n < 50:
			b.WriteString("\r")
		case n < 55:
			b.WriteString("\b")
		case n < 58:
			b.WriteString("\t")
		default:
			b.WriteString(randomCSI(r, cols, rows))
		}
	}
	return []byte(b.String())
}

func randomText(r *rand.Rand) string {
	const ascii = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJ0123456789 .,-_/"
	// A few wide and combining characters, because those are where the disagreements live. Not
	// many: a stream that is mostly CJK exercises the width table and little else.
	wide := []string{"日", "本", "語", "한", "é", "é", "ü"}
	var b strings.Builder
	for i := 0; i < 1+r.Intn(8); i++ {
		if r.Intn(8) == 0 {
			b.WriteString(wide[r.Intn(len(wide))])
			continue
		}
		b.WriteByte(ascii[r.Intn(len(ascii))])
	}
	return b.String()
}

// randomCSI produces a sequence from the set this emulator claims to implement, with parameters
// near the edges of the screen - which is where an off-by-one lives.
func randomCSI(r *rand.Rand, cols, rows int) string {
	arg := func(limit int) int {
		switch r.Intn(5) {
		case 0:
			return 1
		case 1:
			return limit
		case 2:
			return limit + 1 // deliberately past the edge
		default:
			return 1 + r.Intn(limit)
		}
	}
	switch r.Intn(16) {
	case 0:
		return fmt.Sprintf("\x1b[%d;%dH", arg(rows), arg(cols))
	case 1:
		return fmt.Sprintf("\x1b[%dA", arg(rows))
	case 2:
		return fmt.Sprintf("\x1b[%dB", arg(rows))
	case 3:
		return fmt.Sprintf("\x1b[%dC", arg(cols))
	case 4:
		return fmt.Sprintf("\x1b[%dD", arg(cols))
	case 5:
		return fmt.Sprintf("\x1b[%dJ", r.Intn(3))
	case 6:
		return fmt.Sprintf("\x1b[%dK", r.Intn(3))
	case 7:
		return fmt.Sprintf("\x1b[%dL", arg(rows))
	case 8:
		return fmt.Sprintf("\x1b[%dM", arg(rows))
	case 9:
		return fmt.Sprintf("\x1b[%dP", arg(cols))
	case 10:
		return fmt.Sprintf("\x1b[%d@", arg(cols))
	case 11:
		return fmt.Sprintf("\x1b[%dX", arg(cols))
	case 12:
		top := 1 + r.Intn(max(rows-1, 1))
		return fmt.Sprintf("\x1b[%d;%dr", top, top+r.Intn(max(rows-top, 1)))
	case 13:
		return fmt.Sprintf("\x1b[%dS", 1+r.Intn(3))
	case 14:
		return fmt.Sprintf("\x1b[%dm", []int{0, 1, 4, 7, 31, 32, 44, 94, 39, 49}[r.Intn(10)])
	default:
		return "\x1b7" // save cursor; the restore comes from another draw of the dice
	}
}

// Shrink makes a failing input smaller while it still fails.
//
// Delta debugging, coarsely: cut the stream in halves, then quarters, and keep any cut that still
// disagrees. A hundred-byte case that a person can read is worth more than a thousand-byte one
// that only a machine can, and the difference decides whether the bug gets fixed or filed.
//
// fails is asked whether a candidate still disagrees. It is expected to be slow - it runs tmux -
// so the number of attempts is bounded rather than exhaustive.
func Shrink(input []byte, fails func([]byte) bool) []byte {
	best := input
	for chunk := len(best) / 2; chunk > 0; chunk /= 2 {
		for i := 0; i+chunk <= len(best); {
			candidate := make([]byte, 0, len(best)-chunk)
			candidate = append(candidate, best[:i]...)
			candidate = append(candidate, best[i+chunk:]...)
			if len(candidate) > 0 && fails(candidate) {
				best = candidate
				continue // the same offset again, now that what follows has moved up
			}
			i += chunk
		}
	}
	return best
}

// AsCase writes bytes in the .in file format, ready to be saved as a corpus case.
//
// So that a disagreement the fuzzer found becomes a case the corpus keeps, rather than a failure
// that scrolls past. Escapes everything that is not plainly printable, because the point of the
// file is being read.
func AsCase(input []byte, cols, rows int, why string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# size %dx%d\n# %s\n", cols, rows, why)
	for _, c := range input {
		switch {
		case c == 0x1b:
			b.WriteString("\\e")
		case c == '\n':
			b.WriteString("\\n")
		case c == '\r':
			b.WriteString("\\r")
		case c == '\t':
			b.WriteString("\\t")
		case c == '\\':
			b.WriteString("\\\\")
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("\n")
	return b.String()
}
