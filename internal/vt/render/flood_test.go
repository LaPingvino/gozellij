package render

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

// Where the rendered attach's two-to-three times actually goes.
//
// The package doc has carried "the cost is not the number of repaints" since the batching
// experiment, which ruled something out without finding anything. These split the work into the
// three things a flood makes something do - move the bytes, interpret them, draw the result - so
// that the answer is a number per part rather than a theory.
//
// Not in `make check`: a benchmark that fails is a slow machine, not a broken program. Run it by
// hand:
//
//	go test ./internal/vt/render -run xxx -bench Flood -benchtime 1x

// floodLines is the size of the flood in the measurement the package doc quotes.
const floodLines = 20000

func flood() []byte {
	var b strings.Builder
	for i := 0; i < floodLines; i++ {
		fmt.Fprintf(&b, "line %d: the quick brown fox jumps over the lazy dog\r\n", i)
	}
	return []byte(b.String())
}

// BenchmarkFloodPipe is what the byte pipe does with the same bytes: hand them on, unread.
func BenchmarkFloodPipe(b *testing.B) {
	data := flood()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		_, _ = io.Copy(io.Discard, strings.NewReader(string(data)))
	}
}

// BenchmarkFloodEmulate is the grid reading every byte of it.
func BenchmarkFloodEmulate(b *testing.B) {
	data := flood()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		t := grid.New(80, 24)
		t.SetScrollback(2000)
		_, _ = t.Write(data)
	}
}

// BenchmarkFloodEmulateNoScrollback separates keeping the history from interpreting the bytes.
// Twenty thousand lines through a twenty-four row screen is twenty thousand lines pushed into the
// scrollback, which is the one part of the work that grows with the flood rather than with the
// screen.
func BenchmarkFloodEmulateNoScrollback(b *testing.B) {
	data := flood()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		t := grid.New(80, 24)
		t.SetScrollback(0)
		_, _ = t.Write(data)
	}
}

// BenchmarkFloodPaint is one whole-screen repaint, which is what the flood causes a few of.
func BenchmarkFloodPaint(b *testing.B) {
	t := grid.New(80, 24)
	t.SetScrollback(2000)
	_, _ = t.Write(flood())
	b.ResetTimer()
	for range b.N {
		_ = Screen(t)
	}
}
